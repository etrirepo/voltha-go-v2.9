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

package types

import (
//	"github.com/google/gopacket"
	"github.com/opencord/voltha-protos/v5/go/onossliceservice"
//	"net"
)

type MessageType int

const (
	DeviceStatusResponse     MessageType = 0
)

func (m MessageType) String() string {
	names := [...]string{
		"DeviceStatusResponse",
	}
	return names[m]
}

type Message struct {
	Type MessageType
	Data interface{}
}

//type DeviceStatusResponseMessage struct {
//  DeviceStatusResponse DeviceStatusResponseMsg
//}
//
type DeviceStatusResponseMessage struct{
    Identifier string
    ParentId string
    Type onossliceservice.DeviceType
    Status onossliceservice.DeviceStatus
    PortStatus []*onossliceservice.PortStatus
}

