/*
 * Copyright 2018-present Open Networking Foundation

 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at

 * http://www.apache.org/licenses/LICENSE-2.0

 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package device

import (
	"context"
	"fmt"
	"sync"
	"time"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/opencord/voltha-go/rw_core/config"
	"github.com/opencord/voltha-lib-go/v7/pkg/probe"
	"github.com/opencord/voltha-protos/v5/go/core"

	"github.com/opencord/voltha-go/db/model"
	"github.com/opencord/voltha-go/rw_core/core/adapter"
	"github.com/opencord/voltha-go/rw_core/core/device/event"
	"github.com/opencord/voltha-go/rw_core/core/device/state"
	"github.com/opencord/voltha-go/rw_core/utils"
	"github.com/opencord/voltha-lib-go/v7/pkg/events"
	"github.com/opencord/voltha-lib-go/v7/pkg/log"
	ca "github.com/opencord/voltha-protos/v5/go/core_adapter"
	ofp "github.com/opencord/voltha-protos/v5/go/openflow_13"
	"github.com/opencord/voltha-protos/v5/go/voltha"
	"github.com/opencord/voltha-protos/v5/go/bossopenolt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Manager represent device manager attributes
type Manager struct {
	deviceAgents      sync.Map
	rootDevices       map[string]bool
	lockRootDeviceMap sync.RWMutex
	*event.Agent
	adapterMgr              *adapter.Manager
	logicalDeviceMgr        *LogicalManager
	stateTransitions        *state.TransitionMap
	dbPath                  *model.Path
	dProxy                  *model.Proxy
	coreInstanceID          string
	internalTimeout         time.Duration
	rpcTimeout              time.Duration
	flowTimeout             time.Duration
	devicesLoadingLock      sync.RWMutex
	deviceLoadingInProgress map[string][]chan int
	config                  *config.RWCoreFlags
}
type BossOpenoltManager struct {
        DeviceManager *Manager
}
func GetNewBossOpenoltManager(deviceManager *Manager) *BossOpenoltManager {
        return &BossOpenoltManager{DeviceManager: deviceManager}
}

//NewManagers creates the Manager and the Logical Manager.
func NewManagers(dbPath *model.Path, adapterMgr *adapter.Manager, cf *config.RWCoreFlags, coreInstanceID string, eventProxy *events.EventProxy) (*Manager, *LogicalManager) {
	deviceMgr := &Manager{
		rootDevices:             make(map[string]bool),
		coreInstanceID:          coreInstanceID,
		dbPath:                  dbPath,
		dProxy:                  dbPath.Proxy("devices"),
		adapterMgr:              adapterMgr,
		internalTimeout:         cf.InternalTimeout,
		rpcTimeout:              cf.RPCTimeout,
		flowTimeout:             cf.FlowTimeout,
		Agent:                   event.NewAgent(eventProxy, coreInstanceID, cf.VolthaStackID),
		deviceLoadingInProgress: make(map[string][]chan int),
		config:                  cf,
	}
	deviceMgr.stateTransitions = state.NewTransitionMap(deviceMgr)

	logicalDeviceMgr := &LogicalManager{
		Manager:                        event.NewManager(eventProxy, coreInstanceID, cf.VolthaStackID),
		deviceMgr:                      deviceMgr,
		dbPath:                         dbPath,
		ldProxy:                        dbPath.Proxy("logical_devices"),
		internalTimeout:                cf.InternalTimeout,
		logicalDeviceLoadingInProgress: make(map[string][]chan int),
	}
	deviceMgr.logicalDeviceMgr = logicalDeviceMgr

	adapterMgr.SetAdapterRestartedCallback(deviceMgr.adapterRestartedHandler)

	return deviceMgr, logicalDeviceMgr
}

func (dMgr *Manager) Start(ctx context.Context, serviceName string) error {
	logger.Info(ctx, "starting-device-manager")
	probe.UpdateStatusFromContext(ctx, serviceName, probe.ServiceStatusPreparing)

	// Load all the devices from the dB
	var devices []*voltha.Device
	if err := dMgr.dProxy.List(ctx, &devices); err != nil {
		// Any error from the dB means if we proceed we may end up with corrupted data
		logger.Errorw(ctx, "failed-to-list-devices-from-KV", log.Fields{"error": err, "service-name": serviceName})
		return err
	}

	defer probe.UpdateStatusFromContext(ctx, serviceName, probe.ServiceStatusRunning)

	if len(devices) == 0 {
		logger.Info(ctx, "no-device-to-load")
		return nil
	}

	for _, device := range devices {
		// Create an agent for each device
		agent := newAgent(device, dMgr, dMgr.dbPath, dMgr.dProxy, dMgr.internalTimeout, dMgr.rpcTimeout, dMgr.flowTimeout)
		if _, err := agent.start(ctx, true, device); err != nil {
			logger.Warnw(ctx, "failure-starting-agent", log.Fields{"device-id": device.Id})
		} else {
			dMgr.addDeviceAgentToMap(agent)
		}
	}

	// TODO: Need to trigger a reconcile at this point

	logger.Info(ctx, "device-manager-started")

	return nil
}

func (dMgr *Manager) addDeviceAgentToMap(agent *Agent) {
	if _, exist := dMgr.deviceAgents.Load(agent.deviceID); !exist {
		dMgr.deviceAgents.Store(agent.deviceID, agent)
	}
	dMgr.lockRootDeviceMap.Lock()
	defer dMgr.lockRootDeviceMap.Unlock()
	dMgr.rootDevices[agent.deviceID] = agent.isRootDevice

}

func (dMgr *Manager) deleteDeviceAgentFromMap(agent *Agent) {
	dMgr.deviceAgents.Delete(agent.deviceID)
	dMgr.lockRootDeviceMap.Lock()
	defer dMgr.lockRootDeviceMap.Unlock()
	delete(dMgr.rootDevices, agent.deviceID)
}

// getDeviceAgent returns the agent managing the device.  If the device is not in memory, it will loads it, if it exists
func (dMgr *Manager) getDeviceAgent(ctx context.Context, deviceID string) *Agent {
	agent, ok := dMgr.deviceAgents.Load(deviceID)
	if ok {
		return agent.(*Agent)
	}
	// Try to load into memory - loading will also create the device agent and set the device ownership
	err := dMgr.load(ctx, deviceID)
	if err == nil {
		agent, ok = dMgr.deviceAgents.Load(deviceID)
		if !ok {
			return nil
		}
		return agent.(*Agent)
	}
	//TODO: Change the return params to return an error as well
	logger.Errorw(ctx, "loading-device-failed", log.Fields{"device-id": deviceID, "error": err})
	return nil
}

// listDeviceIdsFromMap returns the list of device IDs that are in memory
func (dMgr *Manager) listDeviceIdsFromMap() *voltha.IDs {
	result := &voltha.IDs{Items: make([]*voltha.ID, 0)}

	dMgr.deviceAgents.Range(func(key, value interface{}) bool {
		result.Items = append(result.Items, &voltha.ID{Id: key.(string)})
		return true
	})

	return result
}

// stopManagingDevice stops the management of the device as well as any of its reference device and logical device.
// This function is called only in the Core that does not own this device.  In the Core that owns this device then a
// deletion deletion also includes removal of any reference of this device.
func (dMgr *Manager) stopManagingDevice(ctx context.Context, id string) {
	logger.Infow(ctx, "stop-managing-device", log.Fields{"device-id": id})
	if dMgr.IsDeviceInCache(id) { // Proceed only if an agent is present for this device
		if device, err := dMgr.getDeviceReadOnly(ctx, id); err == nil && device.Root {
			// stop managing the logical device
			_ = dMgr.logicalDeviceMgr.stopManagingLogicalDeviceWithDeviceID(ctx, id)
		}
		if agent := dMgr.getDeviceAgent(ctx, id); agent != nil {
			if err := agent.stop(ctx); err != nil {
				logger.Warnw(ctx, "unable-to-stop-device-agent", log.Fields{"device-id": agent.deviceID, "error": err})
			}
			dMgr.deleteDeviceAgentFromMap(agent)
		}
	}
}

// getDeviceReadOnly will returns a device, either from memory or from the dB, if present
func (dMgr *Manager) getDeviceReadOnly(ctx context.Context, id string) (*voltha.Device, error) {
	logger.Debugw(ctx, "get-device-read-only", log.Fields{"device-id": id})
	if agent := dMgr.getDeviceAgent(ctx, id); agent != nil {
		return agent.getDeviceReadOnly(ctx)
	}
	return nil, status.Errorf(codes.NotFound, "%s", id)
}

func (dMgr *Manager) listDevicePorts(ctx context.Context, id string) (map[uint32]*voltha.Port, error) {
	logger.Debugw(ctx, "list-device-ports", log.Fields{"device-id": id})
	agent := dMgr.getDeviceAgent(ctx, id)
	if agent == nil {
		return nil, status.Errorf(codes.NotFound, "%s", id)
	}
	return agent.listDevicePorts(), nil
}

// IsDeviceInCache returns true if device is found in the map
func (dMgr *Manager) IsDeviceInCache(id string) bool {
	_, exist := dMgr.deviceAgents.Load(id)
	return exist
}

//isParentDeviceExist checks whether device is already preprovisioned.
func (dMgr *Manager) isParentDeviceExist(ctx context.Context, newDevice *voltha.Device) (bool, error) {
	hostPort := newDevice.GetHostAndPort()
	var devices []*voltha.Device
	if err := dMgr.dProxy.List(ctx, &devices); err != nil {
		logger.Errorw(ctx, "failed-to-list-devices-from-cluster-data-proxy", log.Fields{"error": err})
		return false, err
	}
	for _, device := range devices {
		if !device.Root {
			continue
		}

		if hostPort != "" && hostPort == device.GetHostAndPort() {
			return true, nil
		}
		if newDevice.MacAddress != "" && newDevice.MacAddress == device.MacAddress {
			return true, nil
		}
	}
	return false, nil
}

//getDeviceFromModelretrieves the device data from the model.
func (dMgr *Manager) getDeviceFromModel(ctx context.Context, deviceID string) (*voltha.Device, error) {
	device := &voltha.Device{}
	if have, err := dMgr.dProxy.Get(ctx, deviceID, device); err != nil {
		logger.Errorw(ctx, "failed-to-get-device-info-from-cluster-proxy", log.Fields{"error": err})
		return nil, err
	} else if !have {
		return nil, status.Error(codes.NotFound, deviceID)
	}

	return device, nil
}

// loadDevice loads the deviceID in memory, if not present
func (dMgr *Manager) loadDevice(ctx context.Context, deviceID string) (*Agent, error) {
	if deviceID == "" {
		return nil, status.Error(codes.InvalidArgument, "deviceId empty")
	}
	var err error
	var device *voltha.Device
	dMgr.devicesLoadingLock.Lock()
	if _, exist := dMgr.deviceLoadingInProgress[deviceID]; !exist {
		if !dMgr.IsDeviceInCache(deviceID) {
			dMgr.deviceLoadingInProgress[deviceID] = []chan int{make(chan int, 1)}
			dMgr.devicesLoadingLock.Unlock()
			// Proceed with the loading only if the device exist in the Model (could have been deleted)
			if device, err = dMgr.getDeviceFromModel(ctx, deviceID); err == nil {
				logger.Debugw(ctx, "loading-device", log.Fields{"device-id": deviceID})
				agent := newAgent(device, dMgr, dMgr.dbPath, dMgr.dProxy, dMgr.internalTimeout, dMgr.rpcTimeout, dMgr.flowTimeout)
				if _, err = agent.start(ctx, true, device); err != nil {
					logger.Warnw(ctx, "failure-loading-device", log.Fields{"device-id": deviceID, "error": err})
				} else {
					dMgr.addDeviceAgentToMap(agent)
				}
			} else {
				logger.Debugw(ctx, "device-is-not-in-model", log.Fields{"device-id": deviceID})
			}
			// announce completion of task to any number of waiting channels
			dMgr.devicesLoadingLock.Lock()
			if v, ok := dMgr.deviceLoadingInProgress[deviceID]; ok {
				for _, ch := range v {
					close(ch)
				}
				delete(dMgr.deviceLoadingInProgress, deviceID)
			}
			dMgr.devicesLoadingLock.Unlock()
		} else {
			dMgr.devicesLoadingLock.Unlock()
		}
	} else {
		ch := make(chan int, 1)
		dMgr.deviceLoadingInProgress[deviceID] = append(dMgr.deviceLoadingInProgress[deviceID], ch)
		dMgr.devicesLoadingLock.Unlock()
		//	Wait for the channel to be closed, implying the process loading this device is done.
		<-ch
	}
	if agent, ok := dMgr.deviceAgents.Load(deviceID); ok {
		return agent.(*Agent), nil
	}
	return nil, status.Errorf(codes.Aborted, "Error loading device %s", deviceID)
}

// loadRootDeviceParentAndChildren loads the children and parents of a root device in memory
func (dMgr *Manager) loadRootDeviceParentAndChildren(ctx context.Context, device *voltha.Device, devicePorts map[uint32]*voltha.Port) error {
	logger.Debugw(ctx, "loading-parent-and-children", log.Fields{"device-id": device.Id})
	if device.Root {
		// Scenario A
		if device.ParentId != "" {
			// Load logical device if needed.
			if err := dMgr.logicalDeviceMgr.load(ctx, device.ParentId); err != nil {
				logger.Warnw(ctx, "failure-loading-logical-device", log.Fields{"logical-device-id": device.ParentId})
			}
		} else {
			logger.Debugw(ctx, "no-parent-to-load", log.Fields{"device-id": device.Id})
		}
		// Load all child devices, if needed
		childDeviceIds := dMgr.getAllChildDeviceIds(ctx, devicePorts)
		for childDeviceID := range childDeviceIds {
			if _, err := dMgr.loadDevice(ctx, childDeviceID); err != nil {
				logger.Warnw(ctx, "failure-loading-device", log.Fields{"device-id": childDeviceID, "error": err})
				return err
			}
		}
		logger.Debugw(ctx, "loaded-children", log.Fields{"device-id": device.Id, "num-children": len(childDeviceIds)})
	}
	return nil
}

// load loads the deviceId in memory, if not present, and also loads its accompanying parents and children.  Loading
// in memory is for improved performance.  It is not imperative that a device needs to be in memory when a request
// acting on the device is received by the core. In such a scenario, the Core will load the device in memory first
// and the proceed with the request.
func (dMgr *Manager) load(ctx context.Context, deviceID string) error {
	logger.Debug(ctx, "load...")
	// First load the device - this may fail in case the device was deleted intentionally by the other core
	var dAgent *Agent
	var err error
	if dAgent, err = dMgr.loadDevice(ctx, deviceID); err != nil {
		return err
	}
	// Get the loaded device details
	device, err := dAgent.getDeviceReadOnly(ctx)
	if err != nil {
		return err
	}

	// If the device is in Pre-provisioning or getting deleted state stop here
	if device.AdminState == voltha.AdminState_PREPROVISIONED || dAgent.isDeletionInProgress() {
		return nil
	}

	// Now we face two scenarios
	if device.Root {
		devicePorts := dAgent.listDevicePorts()

		// Load all children as well as the parent of this device (logical_device)
		if err := dMgr.loadRootDeviceParentAndChildren(ctx, device, devicePorts); err != nil {
			logger.Warnw(ctx, "failure-loading-device-parent-and-children", log.Fields{"device-id": deviceID})
			return err
		}
		logger.Debugw(ctx, "successfully-loaded-parent-and-children", log.Fields{"device-id": deviceID})
	} else if device.ParentId != "" {
		//	Scenario B - use the parentId of that device (root device) to trigger the loading
		return dMgr.load(ctx, device.ParentId)
	}

	return nil
}

// adapterRestarted is invoked whenever an adapter is restarted
func (dMgr *Manager) adapterRestarted(ctx context.Context, adapter *voltha.Adapter) error {
	logger.Debugw(ctx, "adapter-restarted", log.Fields{"adapter-id": adapter.Id, "vendor": adapter.Vendor,
		"current-replica": adapter.CurrentReplica, "total-replicas": adapter.TotalReplicas, "restarted-endpoint": adapter.Endpoint})

	numberOfDevicesToReconcile := 0
	dMgr.deviceAgents.Range(func(key, value interface{}) bool {
		deviceAgent, ok := value.(*Agent)
		if ok && deviceAgent.adapterEndpoint == adapter.Endpoint {
			// Before reconciling, abort in-process request
			if err := deviceAgent.abortAllProcessing(utils.WithNewSpanAndRPCMetadataContext(ctx, "AbortProcessingOnRestart")); err == nil {
				logger.Debugw(ctx, "reconciling-device",
					log.Fields{
						"device-id":          deviceAgent.deviceID,
						"root-device":        deviceAgent.isRootDevice,
						"restarted-endpoint": adapter.Endpoint,
						"device-type":        deviceAgent.deviceType,
						"adapter-type":       adapter.Type,
					})
				go deviceAgent.ReconcileDevice(utils.WithNewSpanAndRPCMetadataContext(ctx, "ReconcileDevice"))
				numberOfDevicesToReconcile++
			} else {
				logger.Errorw(ctx, "failed-aborting-exisiting-processing", log.Fields{"error": err})
			}
		}
		return true
	})
	logger.Debugw(ctx, "reconciling-on-adapter-restart-initiated", log.Fields{"adapter-endpoint": adapter.Endpoint, "number-of-devices-to-reconcile": numberOfDevicesToReconcile})
	return nil
}

func (dMgr *Manager) UpdateDeviceUsingAdapterData(ctx context.Context, device *voltha.Device) error {
	logger.Debugw(ctx, "update-device-using-adapter-data", log.Fields{"device-id": device.Id, "device": device})
	if agent := dMgr.getDeviceAgent(ctx, device.Id); agent != nil {
		return agent.updateDeviceUsingAdapterData(ctx, device)
	}
	return status.Errorf(codes.NotFound, "%s", device.Id)
}

func (dMgr *Manager) addPeerPort(ctx context.Context, deviceID string, port *voltha.Port) error {
	meAsPeer := &voltha.Port_PeerPort{DeviceId: deviceID, PortNo: port.PortNo}
	for _, peerPort := range port.Peers {
		if agent := dMgr.getDeviceAgent(ctx, peerPort.DeviceId); agent != nil {
			if err := agent.addPeerPort(ctx, meAsPeer); err != nil {
				return err
			}
		}
	}
	// Notify the logical device manager to setup a logical port, if needed.  If the added port is an NNI or UNI
	// then a logical port will be added to the logical device and the device route generated.  If the port is a
	// PON port then only the device graph will be generated.
	device, err := dMgr.getDeviceReadOnly(ctx, deviceID)
	if err != nil {
		return err
	}
	ports, err := dMgr.listDevicePorts(ctx, deviceID)
	if err != nil {
		return err
	}
	subCtx := utils.WithSpanAndRPCMetadataFromContext(ctx)

	if err = dMgr.logicalDeviceMgr.updateLogicalPort(subCtx, device, ports, port); err != nil {
		return err
	}
	return nil
}

func (dMgr *Manager) AddPort(ctx context.Context, deviceID string, port *voltha.Port) error {
	agent := dMgr.getDeviceAgent(ctx, deviceID)
	if agent != nil {
		if err := agent.addPort(ctx, port); err != nil {
			return err
		}
		//	Setup peer ports in its own routine
		go func() {
			subCtx := utils.WithSpanAndRPCMetadataFromContext(ctx)
			if err := dMgr.addPeerPort(subCtx, deviceID, port); err != nil {
				logger.Errorw(ctx, "unable-to-add-peer-port", log.Fields{"error": err, "device-id": deviceID})
			}
		}()
		return nil
	}
	return status.Errorf(codes.NotFound, "%s", deviceID)
}

func (dMgr *Manager) canMultipleAdapterRequestProceed(ctx context.Context, deviceIDs []string) error {
	ready := len(deviceIDs) > 0
	for _, deviceID := range deviceIDs {
		agent := dMgr.getDeviceAgent(ctx, deviceID)
		if agent == nil {
			logger.Errorw(ctx, "adapter-nil", log.Fields{"device-id": deviceID})
			return status.Errorf(codes.Unavailable, "adapter-nil-for-%s", deviceID)
		}
		ready = ready && agent.isAdapterConnectionUp(ctx)
		if !ready {
			return status.Errorf(codes.Unavailable, "adapter-connection-down-for-%s", deviceID)
		}
		if err := agent.canDeviceRequestProceed(ctx); err != nil {
			return err
		}
		// Perform the same checks for parent device
		if !agent.isRootDevice {
			parentDeviceAgent := dMgr.getDeviceAgent(ctx, agent.parentID)
			if parentDeviceAgent == nil {
				logger.Errorw(ctx, "parent-device-adapter-nil", log.Fields{"parent-id": agent.parentID})
				return status.Errorf(codes.Unavailable, "parent-device-adapter-nil-for-%s", deviceID)
			}
			if err := parentDeviceAgent.canDeviceRequestProceed(ctx); err != nil {
				return err
			}
		}

	}
	if !ready {
		return status.Error(codes.Unavailable, "adapter(s)-not-ready")
	}
	return nil
}

func (dMgr *Manager) addFlowsAndGroups(ctx context.Context, deviceID string, flows []*ofp.OfpFlowStats, groups []*ofp.OfpGroupEntry, flowMetadata *ofp.FlowMetadata) error {
	logger.Debugw(ctx, "add-flows-and-groups", log.Fields{"device-id": deviceID, "groups:": groups, "flow-metadata": flowMetadata})
	if agent := dMgr.getDeviceAgent(ctx, deviceID); agent != nil {
		return agent.addFlowsAndGroups(ctx, flows, groups, flowMetadata)
	}
	return status.Errorf(codes.NotFound, "%s", deviceID)
}

// deleteParentFlows removes flows from the parent device based on  specific attributes
func (dMgr *Manager) deleteParentFlows(ctx context.Context, deviceID string, uniPort uint32, metadata *ofp.FlowMetadata) error {
	logger.Debugw(ctx, "delete-parent-flows", log.Fields{"device-id": deviceID, "uni-port": uniPort, "metadata": metadata})
	if agent := dMgr.getDeviceAgent(ctx, deviceID); agent != nil {
		if !agent.isRootDevice {
			return status.Errorf(codes.FailedPrecondition, "not-a-parent-device-%s", deviceID)
		}
		return agent.filterOutFlows(ctx, uniPort, metadata)
	}
	return status.Errorf(codes.NotFound, "%s", deviceID)
}

func (dMgr *Manager) deleteFlowsAndGroups(ctx context.Context, deviceID string, flows []*ofp.OfpFlowStats, groups []*ofp.OfpGroupEntry, flowMetadata *ofp.FlowMetadata) error {
	logger.Debugw(ctx, "delete-flows-and-groups", log.Fields{"device-id": deviceID})
	if agent := dMgr.getDeviceAgent(ctx, deviceID); agent != nil {
		return agent.deleteFlowsAndGroups(ctx, flows, groups, flowMetadata)
	}
	return status.Errorf(codes.NotFound, "%s", deviceID)
}

func (dMgr *Manager) updateFlowsAndGroups(ctx context.Context, deviceID string, flows []*ofp.OfpFlowStats, groups []*ofp.OfpGroupEntry, flowMetadata *ofp.FlowMetadata) error {
	logger.Debugw(ctx, "update-flows-and-groups", log.Fields{"device-id": deviceID})
	if agent := dMgr.getDeviceAgent(ctx, deviceID); agent != nil {
		return agent.updateFlowsAndGroups(ctx, flows, groups, flowMetadata)
	}
	return status.Errorf(codes.NotFound, "%s", deviceID)
}

// InitPmConfigs initialize the pm configs as defined by the adapter.
func (dMgr *Manager) InitPmConfigs(ctx context.Context, deviceID string, pmConfigs *voltha.PmConfigs) error {
	if pmConfigs.Id == "" {
		return status.Errorf(codes.FailedPrecondition, "invalid-device-Id")
	}
	if agent := dMgr.getDeviceAgent(ctx, deviceID); agent != nil {
		return agent.initPmConfigs(ctx, pmConfigs)
	}
	return status.Errorf(codes.NotFound, "%s", deviceID)
}

func (dMgr *Manager) getSwitchCapability(ctx context.Context, deviceID string) (*ca.SwitchCapability, error) {
	logger.Debugw(ctx, "get-switch-capability", log.Fields{"device-id": deviceID})
	if agent := dMgr.getDeviceAgent(ctx, deviceID); agent != nil {
		return agent.getSwitchCapability(ctx)
	}
	return nil, status.Errorf(codes.NotFound, "%s", deviceID)
}

func (dMgr *Manager) UpdateDeviceStatus(ctx context.Context, deviceID string, operStatus voltha.OperStatus_Types, connStatus voltha.ConnectStatus_Types) error {
	logger.Debugw(ctx, "update-device-status", log.Fields{"device-id": deviceID, "oper-status": operStatus, "conn-status": connStatus})
	if agent := dMgr.getDeviceAgent(ctx, deviceID); agent != nil {
		return agent.updateDeviceStatus(ctx, operStatus, connStatus)
	}
	return status.Errorf(codes.NotFound, "%s", deviceID)
}

func (dMgr *Manager) UpdateChildrenStatus(ctx context.Context, deviceID string, operStatus voltha.OperStatus_Types, connStatus voltha.ConnectStatus_Types) error {
	logger.Debugw(ctx, "update-children-status", log.Fields{"parent-device-id": deviceID, "oper-status": operStatus, "conn-status": connStatus})
	parentDevicePorts, err := dMgr.listDevicePorts(ctx, deviceID)
	if err != nil {
		return status.Errorf(codes.Aborted, "%s", err.Error())
	}
	for childDeviceID := range dMgr.getAllChildDeviceIds(ctx, parentDevicePorts) {
		if agent := dMgr.getDeviceAgent(ctx, childDeviceID); agent != nil {
			if err = agent.updateDeviceStatus(ctx, operStatus, connStatus); err != nil {
				return status.Errorf(codes.Aborted, "childDevice:%s, error:%s", childDeviceID, err.Error())
			}
		}
	}
	return nil
}

func (dMgr *Manager) UpdatePortState(ctx context.Context, deviceID string, portType voltha.Port_PortType, portNo uint32, operStatus voltha.OperStatus_Types) error {
	logger.Debugw(ctx, "update-port-state", log.Fields{"device-id": deviceID, "port-type": portType, "port-no": portNo, "oper-status": operStatus})
	if agent := dMgr.getDeviceAgent(ctx, deviceID); agent != nil {
		if err := agent.updatePortState(ctx, portType, portNo, operStatus); err != nil {
			logger.Errorw(ctx, "updating-port-state-failed", log.Fields{"device-id": deviceID, "port-no": portNo, "error": err})
			return err
		}
		// Notify the logical device manager to change the port state
		// Do this for NNI and UNIs only. PON ports are not known by logical device
		if portType == voltha.Port_ETHERNET_NNI || portType == voltha.Port_ETHERNET_UNI {
			go func() {
				subCtx := utils.WithSpanAndRPCMetadataFromContext(ctx)
				err := dMgr.logicalDeviceMgr.updatePortState(subCtx, deviceID, portNo, operStatus)
				if err != nil {
					// While we want to handle (catch) and log when
					// an update to a port was not able to be
					// propagated to the logical port, we can report
					// it as a warning and not an error because it
					// doesn't stop or modify processing.
					// TODO: VOL-2707
					logger.Warnw(ctx, "unable-to-update-logical-port-state", log.Fields{"error": err})
				}
			}()
		}
		return nil
	}
	return status.Errorf(codes.NotFound, "%s", deviceID)
}

//UpdatePortsState updates all ports on the device
func (dMgr *Manager) UpdatePortsState(ctx context.Context, deviceID string, portTypeFilter uint32, state voltha.OperStatus_Types) error {
	logger.Debugw(ctx, "update-ports-state", log.Fields{"device-id": deviceID})
	agent := dMgr.getDeviceAgent(ctx, deviceID)
	if agent == nil {
		return status.Errorf(codes.NotFound, "%s", deviceID)
	}
	if state != voltha.OperStatus_ACTIVE && state != voltha.OperStatus_UNKNOWN {
		return status.Error(codes.Unimplemented, "state-change-not-implemented")
	}
	if err := agent.updatePortsOperState(ctx, portTypeFilter, state); err != nil {
		logger.Warnw(ctx, "update-ports-state-failed", log.Fields{"device-id": deviceID, "error": err})
		return err
	}
	return nil
}

func (dMgr *Manager) packetOut(ctx context.Context, deviceID string, outPort uint32, packet *ofp.OfpPacketOut) error {
	logger.Debugw(ctx, "packet-out", log.Fields{"device-id": deviceID, "out-port": outPort})
	if agent := dMgr.getDeviceAgent(ctx, deviceID); agent != nil {
		return agent.packetOut(ctx, outPort, packet)
	}
	return status.Errorf(codes.NotFound, "%s", deviceID)
}

// PacketIn receives packet from adapter
func (dMgr *Manager) PacketIn(ctx context.Context, deviceID string, port uint32, transactionID string, packet []byte) error {
	logger.Debugw(ctx, "packet-in", log.Fields{"device-id": deviceID, "port": port})
	// Get the logical device Id based on the deviceId
	var device *voltha.Device
	var err error
	if device, err = dMgr.getDeviceReadOnly(ctx, deviceID); err != nil {
		logger.Errorw(ctx, "device-not-found", log.Fields{"device-id": deviceID})
		return err
	}
	if !device.Root {
		logger.Errorw(ctx, "device-not-root", log.Fields{"device-id": deviceID})
		return status.Errorf(codes.FailedPrecondition, "%s", deviceID)
	}

	if err := dMgr.logicalDeviceMgr.packetIn(ctx, device.ParentId, port, packet); err != nil {
		return err
	}
	return nil
}

func (dMgr *Manager) setParentID(ctx context.Context, device *voltha.Device, parentID string) error {
	logger.Debugw(ctx, "set-parent-id", log.Fields{"device-id": device.Id, "parent-id": parentID})
	if agent := dMgr.getDeviceAgent(ctx, device.Id); agent != nil {
		return agent.setParentID(ctx, device, parentID)
	}
	return status.Errorf(codes.NotFound, "%s", device.Id)
}

func (dMgr *Manager) getParentDevice(ctx context.Context, childDevice *voltha.Device) *voltha.Device {
	//	Sanity check
	if childDevice.Root {
		// childDevice is the parent device
		return childDevice
	}
	parentDevice, _ := dMgr.getDeviceReadOnly(ctx, childDevice.ParentId)
	return parentDevice
}

/*
All the functions below are callback functions where they are invoked with the latest and previous data.  We can
therefore use the data as is without trying to get the latest from the model.
*/

//DisableAllChildDevices is invoked as a callback when the parent device is disabled
func (dMgr *Manager) DisableAllChildDevices(ctx context.Context, parentCurrDevice *voltha.Device) error {
	logger.Debug(ctx, "disable-all-child-devices")
	ports, _ := dMgr.listDevicePorts(ctx, parentCurrDevice.Id)
	for childDeviceID := range dMgr.getAllChildDeviceIds(ctx, ports) {
		if agent := dMgr.getDeviceAgent(ctx, childDeviceID); agent != nil {
			if err := agent.disableDevice(ctx); err != nil {
				// Just log the error - this error happens only if the child device was already in deleted state.
				logger.Errorw(ctx, "failure-disable-device", log.Fields{"device-id": childDeviceID, "error": err.Error()})
			}
		}
	}
	return nil
}

//getAllChildDeviceIds is a helper method to get all the child device IDs from the device passed as parameter
func (dMgr *Manager) getAllChildDeviceIds(ctx context.Context, parentDevicePorts map[uint32]*voltha.Port) map[string]struct{} {
	logger.Debug(ctx, "get-all-child-device-ids")
	childDeviceIds := make(map[string]struct{}, len(parentDevicePorts))
	for _, port := range parentDevicePorts {
		for _, peer := range port.Peers {
			childDeviceIds[peer.DeviceId] = struct{}{}
		}
	}
	logger.Debugw(ctx, "returning-getAllChildDeviceIds", log.Fields{"childDeviceIds": childDeviceIds})
	return childDeviceIds
}

//GgtAllChildDevices is a helper method to get all the child device IDs from the device passed as parameter
func (dMgr *Manager) getAllChildDevices(ctx context.Context, parentDeviceID string) (*voltha.Devices, error) {
	logger.Debugw(ctx, "get-all-child-devices", log.Fields{"parent-device-id": parentDeviceID})
	if parentDevicePorts, err := dMgr.listDevicePorts(ctx, parentDeviceID); err == nil {
		childDevices := make([]*voltha.Device, 0)
		for deviceID := range dMgr.getAllChildDeviceIds(ctx, parentDevicePorts) {
			if d, e := dMgr.getDeviceReadOnly(ctx, deviceID); e == nil && d != nil {
				childDevices = append(childDevices, d)
			}
		}
		return &voltha.Devices{Items: childDevices}, nil
	}
	return nil, status.Errorf(codes.NotFound, "%s", parentDeviceID)
}

func (dMgr *Manager) NotifyInvalidTransition(ctx context.Context, device *voltha.Device) error {
	logger.Errorw(ctx, "notify-invalid-transition", log.Fields{
		"device":           device.Id,
		"curr-admin-state": device.AdminState,
		"curr-oper-state":  device.OperStatus,
		"curr-conn-state":  device.ConnectStatus,
	})
	//TODO: notify over kafka?
	return nil
}

// UpdateDeviceAttribute updates value of particular device attribute
func (dMgr *Manager) UpdateDeviceAttribute(ctx context.Context, deviceID string, attribute string, value interface{}) {
	if agent, ok := dMgr.deviceAgents.Load(deviceID); ok {
		agent.(*Agent).updateDeviceAttribute(ctx, attribute, value)
	}
}

// GetParentDeviceID returns parent device id, either from memory or from the dB, if present
func (dMgr *Manager) GetParentDeviceID(ctx context.Context, deviceID string) string {
	if device, _ := dMgr.getDeviceReadOnly(ctx, deviceID); device != nil {
		logger.Infow(ctx, "get-parent-device-id", log.Fields{"device-id": device.Id, "parent-id": device.ParentId})
		return device.ParentId
	}
	return ""
}

func (dMgr *Manager) UpdateDeviceReason(ctx context.Context, deviceID string, reason string) error {
	logger.Debugw(ctx, "update-device-reason", log.Fields{"device-id": deviceID, "reason": reason})
	if agent := dMgr.getDeviceAgent(ctx, deviceID); agent != nil {
		return agent.updateDeviceReason(ctx, reason)
	}
	return status.Errorf(codes.NotFound, "%s", deviceID)
}

func (dMgr *Manager) SendRPCEvent(ctx context.Context, id string, rpcEvent *voltha.RPCEvent,
	category voltha.EventCategory_Types, subCategory *voltha.EventSubCategory_Types, raisedTs int64) {
	//TODO Instead of directly sending to the kafka bus, queue the message and send it asynchronously
	dMgr.Agent.SendRPCEvent(ctx, id, rpcEvent, category, subCategory, raisedTs)
}

func (dMgr *Manager) GetTransientState(ctx context.Context, id string) (core.DeviceTransientState_Types, error) {
	agent := dMgr.getDeviceAgent(ctx, id)
	if agent == nil {
		return core.DeviceTransientState_NONE, status.Errorf(codes.NotFound, "%s", id)
	}
	return agent.getTransientState(), nil
}

func (dMgr *Manager) validateImageDownloadRequest(request *voltha.DeviceImageDownloadRequest) error {
	if request == nil || request.Image == nil || len(request.DeviceId) == 0 {
		return status.Errorf(codes.InvalidArgument, "invalid argument")
	}

	for _, deviceID := range request.DeviceId {
		if deviceID == nil {
			return status.Errorf(codes.InvalidArgument, "id is nil")
		}
	}
	return nil
}

func (dMgr *Manager) validateImageRequest(request *voltha.DeviceImageRequest) error {
	if request == nil || len(request.DeviceId) == 0 || request.DeviceId[0] == nil {
		return status.Errorf(codes.InvalidArgument, "invalid argument")
	}

	for _, deviceID := range request.DeviceId {
		if deviceID == nil {
			return status.Errorf(codes.InvalidArgument, "id is nil")
		}
	}

	return nil
}

func (dMgr *Manager) validateDeviceImageResponse(response *voltha.DeviceImageResponse) error {
	if response == nil || len(response.GetDeviceImageStates()) == 0 || response.GetDeviceImageStates()[0] == nil {
		return status.Errorf(codes.Internal, "invalid-response-from-adapter")
	}

	return nil
}

func (dMgr *Manager) waitForAllResponses(ctx context.Context, opName string, respCh chan []*voltha.DeviceImageState, expectedResps int) (*voltha.DeviceImageResponse, error) {
	response := &voltha.DeviceImageResponse{}
	respCount := 0
	for {
		select {
		case resp, ok := <-respCh:
			if !ok {
				logger.Errorw(ctx, opName+"-failed", log.Fields{"error": "channel-closed"})
				return response, status.Errorf(codes.Aborted, "channel-closed")
			}

			if resp != nil {
				logger.Debugw(ctx, opName+"-result", log.Fields{"image-state": resp[0].GetImageState(), "device-id": resp[0].GetDeviceId()})
				response.DeviceImageStates = append(response.DeviceImageStates, resp...)
			}

			respCount++

			//check whether all responses received, if so, sent back the collated response
			if respCount == expectedResps {
				return response, nil
			}
			continue
		case <-ctx.Done():
			return nil, status.Errorf(codes.Aborted, opName+"-failed-%s", ctx.Err())
		}
	}
}

func (dMgr *Manager) ReconcilingCleanup(ctx context.Context, device *voltha.Device) error {
	agent := dMgr.getDeviceAgent(ctx, device.Id)
	if agent == nil {
		logger.Errorf(ctx, "Not able to get device agent.")
		return status.Errorf(codes.NotFound, "Not able to get device agent for device : %s", device.Id)
	}
	err := agent.reconcilingCleanup(ctx)
	if err != nil {
		logger.Errorf(ctx, err.Error())
		return status.Errorf(codes.Internal, err.Error())
	}
	return nil
}

func (dMgr *Manager) adapterRestartedHandler(ctx context.Context, endpoint string) error {
	// Get the adapter corresponding to that endpoint
	if a, _ := dMgr.adapterMgr.GetAdapterWithEndpoint(ctx, endpoint); a != nil {
		return dMgr.adapterRestarted(ctx, a)
	}
	logger.Errorw(ctx, "restarted-adapter-not-found", log.Fields{"endpoint": endpoint})
	return fmt.Errorf("restarted adapter at endpoint %s not found", endpoint)
}
func(boss *BossOpenoltManager) GetOltConnect(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.OltConnResponse, error){
    /*response :=&bossopenolt.OltConnResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetOltConnect(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetOltDeviceInfo(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.OltDevResponse, error){
    /*response :=&bossopenolt.OltDevResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetOltDeviceInfo(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetPmdTxDis(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetPmdTxDis(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetPmdTxdis(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.PmdTxdisResponse, error){
    /*response :=&bossopenolt.PmdTxdisResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetPmdTxdis(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetDevicePmdStatus(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.PmdStatusResponse, error){
    /*response :=&bossopenolt.PmdStatusResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetDevicePmdStatus(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetDevicePort(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetDevicePort(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "SetDevicePort-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetDevicePort(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.GetPortResponse, error){
    /*response :=&bossopenolt.GetPortResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetDevicePort(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) PortReset(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.PortReset(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetMtuSize(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetMtuSize(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetMtuSize(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.MtuSizeResponse, error){
    /*response :=&bossopenolt.MtuSizeResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetMtuSize(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetVlan(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetVlan(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

//func(boss *BossOpenoltManager) GetVlan(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.GetVlanResponse, error){
//    /*response :=&bossopenolt.GetVlanResponse{
//        DeviceId : reqMessage.DeviceId,
//        VlanMode : 1,
//        Fields : "0x3064",
//    }*/
//    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
//        resp, err := agent.GetVlan(ctx, reqMessage)
//        if err != nil {
//            return nil, err
//        }
//        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
//        return resp, nil
//    }
//    //return response, nil
//    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
//}
//
func(boss *BossOpenoltManager) SetLutMode(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetLutMode(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetLutMode(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ModeResponse, error){
    /*response :=&bossopenolt.ModeResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetLutMode(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetAgingMode(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetAgingMode(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetAgingMode(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ModeResponse, error){
    /*response :=&bossopenolt.ModeResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetAgingMode(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetAgingTime(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetAgingTime(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetAgingTime(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.AgingTimeResponse, error){
    /*response :=&bossopenolt.AgingTimeResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetAgingTime(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetDeviceMacInfo(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.DevMacInfoResponse, error){
    /*response :=&bossopenolt.DevMacInfoResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetDeviceMacInfo(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetSdnTable(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.SdnTableKeyResponse, error){
    /*response :=&bossopenolt.SdnTableKeyResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetSdnTable(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetSdnTable(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.SdnTableResponse, error){
    /*response :=&bossopenolt.SdnTableResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetSdnTable(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetLength(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetLength(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetLength(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.LengthResponse, error){
    /*response :=&bossopenolt.LengthResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetLength(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetQuietZone(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetQuietZone(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetQuietZone(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.QuietZoneResponse, error){
    /*response :=&bossopenolt.QuietZoneResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetQuietZone(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetFecMode(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetFecMode(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetFecMode(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ModeResponse, error){
    /*response :=&bossopenolt.ModeResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetFecMode(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) AddOnu(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.AddOnuResponse, error){
    /*response :=&bossopenolt.AddOnuResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.AddOnu(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) DeleteOnu25G(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.DeleteOnu(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) AddOnuSla(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.AddOnuSla(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) ClearOnuSla(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.ClearOnuSla(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetSlaTable(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.RepeatedSlaResponse, error){
    /*response :=&bossopenolt.RepeatedSlaResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetSlaTable(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetOnuAllocid(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetOnuAllocid(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) DelOnuAllocid(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.DelOnuAllocid(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetOnuVssn(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetOnuVssn(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetOnuVssn(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.OnuVssnResponse, error){
    /*response :=&bossopenolt.OnuVssnResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetOnuVssn(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetOnuDistance(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.OnuDistResponse, error){
    /*response :=&bossopenolt.OnuDistResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetOnuDistance(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetBurstDelimiter(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetBurstDelimiter(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetBurstDelimiter(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.BurstDelimitResponse, error){
    /*response :=&bossopenolt.BurstDelimitResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetBurstDelimiter(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetBurstPreamble(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetBurstPreamble(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetBurstPreamble(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.BurstPreambleResponse, error){
    /*response :=&bossopenolt.BurstPreambleResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetBurstPreamble(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetBurstVersion(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetBurstVersion(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetBurstVersion(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.BurstVersionResponse, error){
    /*response :=&bossopenolt.BurstVersionResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetBurstVersion(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetBurstProfile(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetBurstProfile(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetBurstProfile(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.BurstProfileResponse, error){
    /*response :=&bossopenolt.BurstProfileResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetBurstProfile(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetRegisterStatus(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.RegisterStatusResponse, error){
    /*response :=&bossopenolt.RegisterStatusResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetRegisterStatus(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetOnuInfo(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.OnuInfoResponse, error){
    /*response :=&bossopenolt.OnuInfoResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetOnuInfo(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetOmciStatus(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.StatusResponse, error){
    /*response :=&bossopenolt.StatusResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetOmciStatus(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetDsOmciOnu(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetDsOmciOnu(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetDsOmciData(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetDsOmciData(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetUsOmciData(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.OmciDataResponse, error){
    /*response :=&bossopenolt.OmciDataResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetUsOmciData(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetTod(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetTod(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetTod(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.TodResponse, error){
    /*response :=&bossopenolt.TodResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetTod(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetDataMode(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetDataMode(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetDataMode(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ModeResponse, error){
    /*response :=&bossopenolt.ModeResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetDataMode(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetFecDecMode(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetFecDecMode(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetFecDecMode(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ModeResponse, error){
    /*response :=&bossopenolt.ModeResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetFecDecMode(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetDelimiter(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetDelimiter(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetDelimiter(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.FecDecResponse, error){
    /*response :=&bossopenolt.FecDecResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetDelimiter(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetErrorPermit(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetErrorPermit(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetErrorPermit(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ErrorPermitResponse, error){
    /*response :=&bossopenolt.ErrorPermitResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetErrorPermit(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetPmControl(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetPmControl(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetPmControl(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.PmControlResponse, error){
    /*response :=&bossopenolt.PmControlResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetPmControl(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetPmTable(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.PmTableResponse, error){
    /*response :=&bossopenolt.PmTableResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetPmTable(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetSAOn(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetSAOn(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetSAOff(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetSAOff(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func (dMgr *Manager) CreateDeviceHandler(ctx context.Context, id *voltha.ID) (*empty.Empty, error) {
	log.EnrichSpan(ctx, log.Fields{"device-id": id.Id})

	logger.Debugw(ctx, "CreateDeviceHandler", log.Fields{"device-id": id.Id})
	agent := dMgr.getDeviceAgent(ctx, id.Id)
	if agent == nil {
		return nil, status.Errorf(codes.NotFound, "%s", id.Id)
	}
	return agent.createDeviceHandler(ctx)
}

func(boss *BossOpenoltManager) SetSliceBw(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.ExecResult, error){
    /*response :=&bossopenolt.ExecResult{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetSliceBw(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetSliceBw(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.GetSliceBwResponse, error){
    /*response :=&bossopenolt.GetSliceBwResponse{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetSliceBw(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) SetSlaV2(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.SlaV2Response, error){
    /*response :=&bossopenolt.SlaV2Response{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.SetSlaV2(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

func(boss *BossOpenoltManager) GetSlaV2(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.SlaV2Response, error){
    /*response :=&bossopenolt.SlaV2Response{
        DeviceId : reqMessage.DeviceId,
        VlanMode : 1,
        Fields : "0x3064",
    }*/
    if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
        resp, err := agent.GetSlaV2(ctx, reqMessage)
        if err != nil {
            return nil, err
        }
        logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
        return resp, nil
    }
    //return response, nil
    return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}


func(boss *BossOpenoltManager) GetVlan(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.GetVlanResponse, error){
        /*response :=&bossopenolt.GetVlanResponse{
                DeviceId : reqMessage.DeviceId,
                VlanMode : 1,
                Fields : "0x3064",
        }*/
        if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
                resp, err := agent.getCustomVlan(ctx, reqMessage)
                if err != nil {
                        return nil, err
                }
                logger.Debugw(ctx, "getCustVlan-result", log.Fields{"result": resp})
                return resp, nil
        }
        //return response, nil
        return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}
func(boss *BossOpenoltManager) SendOmciData(ctx context.Context, reqMessage *bossopenolt.BossRequest) (*bossopenolt.BossOmciResponse, error){
        /*response :=&bossopenolt.GetVlanResponse{
                DeviceId : reqMessage.DeviceId,
                VlanMode : 1,
                Fields : "0x3064",
        }*/
        if agent := boss.DeviceManager.getDeviceAgent(ctx, reqMessage.DeviceId); agent != nil {
                resp, err := agent.SendOmciData(ctx, reqMessage)
                if err != nil {
                        return nil, err
                }
                logger.Debugw(ctx, "SendOmciData-result", log.Fields{"result": resp})
                return resp, nil
        }
        //return response, nil
        return nil, status.Errorf(codes.NotFound, "%s", reqMessage.DeviceId)
}

