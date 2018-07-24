// +build linux,opencontrail

/*
 * Copyright (C) 2018 Orange, Inc.
 *
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 *
 */

// When an interface node is created, the VRFID is get from the
// Contrail Vrouter Agent and associated to this node. This VRF is
// then dumped (with rt --dump) to populate the Contrail.RoutingTable
// metadata.
//
// The process rt --monitor is spawn to get route update notifications
// from the Contrail vrouter kernel module. All route updates contain
// the VRFID. This VRFID is then used to get all interface nodes that
// have this VRFID. The Contrail routing table of these nodes is then
// updated according to the route update.
//
// LIMITATION: if the Contrail Vrouter Agent is restated, Skydive
// routing tables are corrupted. Skydive agent then have to be
// restarted when Contrail Vrouter agent is restarted.

package opencontrail

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"

	"github.com/skydive-project/skydive/filters"
	"github.com/skydive-project/skydive/logging"
	"github.com/skydive-project/skydive/topology/graph"
)

// This represents the data we get from rt --monitor stdout
// easyjson:json
type rtMonitorRoute struct {
	Operation string
	Family    string
	VrfId     int `json:"vrf_id"`
	Prefix    int
	Address   string
	NhId      int `json:"nh_id"`
}

const afInetFamily string = "AF_INET"

const OpenContrailRouteProtocol int64 = 200

// The skydive representation of a Contrail route
// easyjson:json
type OpenContrailRoute struct {
	Family   string
	Prefix   string
	NhId     int `json:"NhId"`
	Protocol int64
}

// A VRF contains the list of interface that use this VRF in order to
// be able to garbage collect VRF: if a VRF is no longer associated to
// an interface, this VRF can be deleted.
type RoutingTable struct {
	InterfacesUUID []string
	Routes         []OpenContrailRoute
}

type interfaceUpdate struct {
	InterfaceUUID string
	VrfId         int
}

type routingTableUpdateType int

const (
	AddRoute routingTableUpdateType = iota
	DelRoute
	AddInterface
	DelInterface
)

type RoutingTableUpdate struct {
	action routingTableUpdateType
	route  rtMonitorRoute
	intf   interfaceUpdate
}

// routingTableUpdater serializes route update on both routing tables
// and interfaces.
func (mapper *OpenContrailProbe) routingTableUpdater() {
	var vrfId int
	logging.GetLogger().Debug("Starting routingTableUpdater...")
	for a := range mapper.routingTableUpdaterChan {
		if a.action == AddRoute {
			ocRoute := OpenContrailRoute{
				Protocol: OpenContrailRouteProtocol,
				Prefix:   fmt.Sprintf("%s/%d", a.route.Address, a.route.Prefix),
				Family:   a.route.Family,
				NhId:     a.route.NhId}
			mapper.addRoute(a.route.VrfId, ocRoute)
			vrfId = a.route.VrfId
		} else if a.action == DelRoute {
			ocRoute := OpenContrailRoute{
				Protocol: OpenContrailRouteProtocol,
				Prefix:   fmt.Sprintf("%s/%d", a.route.Address, a.route.Prefix),
				Family:   a.route.Family,
				NhId:     a.route.NhId}
			mapper.delRoute(a.route.VrfId, ocRoute)
			vrfId = a.route.VrfId
		} else if a.action == AddInterface {
			mapper.addInterface(a.intf.VrfId, a.intf.InterfaceUUID)
			vrfId = a.intf.VrfId

		} else if a.action == DelInterface {
			var err error
			if vrfId, err = mapper.deleteInterface(a.intf.InterfaceUUID); err != nil {
				continue
			}
		}
		mapper.onRouteChanged(vrfId)
	}
}

func (mapper *OpenContrailProbe) getOrCreateRoutingTable(vrfId int) *RoutingTable {
	vrf, exists := mapper.routingTables[vrfId]
	if !exists {
		logging.GetLogger().Debugf("Creating a new VRF with ID %d", vrfId)
		itfs := []string{}
		vrf = &RoutingTable{InterfacesUUID: itfs}
		mapper.routingTables[vrfId] = vrf
		err := mapper.vrfInit(vrfId)
		if err != nil {
			logging.GetLogger().Error(err)
		}
	}
	return vrf
}

func (mapper *OpenContrailProbe) addInterface(vrfId int, interfaceUUID string) {
	vrf := mapper.getOrCreateRoutingTable(vrfId)
	logging.GetLogger().Debugf("Appending interface %s to VRF %d...", interfaceUUID, vrfId)
	vrf.InterfacesUUID = append(vrf.InterfacesUUID, interfaceUUID)
}

func (mapper *OpenContrailProbe) OnInterfaceAdded(vrfId int, interfaceUUID string) {
	mapper.routingTableUpdaterChan <- RoutingTableUpdate{action: AddInterface, intf: interfaceUpdate{InterfaceUUID: interfaceUUID, VrfId: vrfId}}
}

// deleteInterface removes interfaces from Vrf. If a Vrf no longer has
// any interfaces, this Vrf is removed.
func (mapper *OpenContrailProbe) deleteInterface(interfaceUUID string) (vrfId int, err error) {
	var found bool
	for k, vrf := range mapper.routingTables {
		for idx, intf := range vrf.InterfacesUUID {
			if intf == interfaceUUID {
				logging.GetLogger().Debugf("Delete interface %s from VRF %d", interfaceUUID, k)
				vrf.InterfacesUUID[idx] = vrf.InterfacesUUID[len(vrf.InterfacesUUID)-1]
				vrf.InterfacesUUID = vrf.InterfacesUUID[:len(vrf.InterfacesUUID)-1]
				found = true
				break
			}
		}
		if found {
			if len(vrf.InterfacesUUID) == 0 {
				logging.GetLogger().Debugf("Delete VRF %d", k)
				delete(mapper.routingTables, k)
			}
		}
	}
	return 0, errors.New("No VrfId was found")
}

func (mapper *OpenContrailProbe) OnInterfaceDeleted(interfaceUUID string) {
	mapper.routingTableUpdaterChan <- RoutingTableUpdate{action: DelInterface, intf: interfaceUpdate{InterfaceUUID: interfaceUUID}}
}

// onRouteChanged writes the Contrail routing table into the
// Contrail.RoutingTable metadata attribute.
func (mapper *OpenContrailProbe) onRouteChanged(vrfId int) {
	vrf := mapper.getOrCreateRoutingTable(vrfId)

	mapper.graph.Lock()
	defer mapper.graph.Unlock()

	filter := graph.NewGraphElementFilter(filters.NewTermInt64Filter("Contrail.VRFID", int64(vrfId)))
	intfs := mapper.graph.GetNodes(filter)

	if len(intfs) == 0 {
		logging.GetLogger().Debugf("No interface with VRF index %d was found (on route add)", vrfId)
		return
	}
	for _, n := range intfs {
		mapper.graph.AddMetadata(n, "Contrail.RoutingTable", vrf.Routes)
		logging.GetLogger().Debugf("Update routes on node %s", n.ID)
	}
}

func (mapper *OpenContrailProbe) addRoute(vrfId int, route OpenContrailRoute) {
	vrf := mapper.getOrCreateRoutingTable(vrfId)
	logging.GetLogger().Debugf("Adding route %v to vrf %d", route, vrfId)
	for _, r := range vrf.Routes {
		if r == route {
			return
		}
	}
	vrf.Routes = append(vrf.Routes, route)
}

func (mapper *OpenContrailProbe) delRoute(vrfId int, route OpenContrailRoute) {
	vrf := mapper.getOrCreateRoutingTable(vrfId)
	for i, r := range vrf.Routes {
		if r.Prefix == route.Prefix {
			logging.GetLogger().Debugf("Removing route %s from vrf %d ", r.Prefix, vrfId)
			vrf.Routes[i] = vrf.Routes[len(vrf.Routes)-1]
			vrf.Routes = vrf.Routes[:len(vrf.Routes)-1]
			return
		}
	}
	logging.GetLogger().Errorf("Can not remove route %v from vrf %d because route has not been found", route, vrfId)
}

// vrfInit uses the Contrail binary rt --dump to get all routes of a VRF.
func (mapper *OpenContrailProbe) vrfInit(vrfId int) error {
	logging.GetLogger().Debugf("Initialisation of VRF %d...", vrfId)

	cmd := exec.Command("rt", "--dump", fmt.Sprint(vrfId))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Start()
	defer cmd.Wait()

	scanner := bufio.NewScanner(stdout)
	separator := regexp.MustCompile("[[:space:]]+")

	// Remove the rt --dump stdout header
	scanner.Scan()
	scanner.Scan()
	scanner.Scan()

	for scanner.Scan() {
		s := separator.Split(scanner.Text(), -1)
		// Ignore non complete entries
		if len(s) != 6 {
			continue
		}

		prefix := s[0]
		nhId, err := strconv.Atoi(s[4])
		if err != nil {
			return err
		}
		// These are not interesting routes
		if nhId == 0 || nhId == 1 {
			continue
		}

		// TODO add family
		mapper.addRoute(vrfId, OpenContrailRoute{
			Protocol: OpenContrailRouteProtocol,
			Prefix:   prefix,
			NhId:     nhId,
			Family:   afInetFamily})
	}
	return nil
}

// We use the binary program "rt" that comes with Contrail to get
// notifications on Contrail route creations and deletions. These
// notifications are broadcasted with Netlink by the linux kernel
// Contrail module. We cannot just listen the Netlink bus because
// messages are encoded with Sandesh which is bound to the Contrail
// version. This is why we read the stdout of the "rt" tools.
func (mapper *OpenContrailProbe) rtMonitor() {
	logging.GetLogger().Debugf("Starting OpenContrail route monitor...")
	cmd := exec.Command("rt", "--monitor")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logging.GetLogger().Debug(err)
	}
	stdoutBuf := bufio.NewReader(stdout)

	rtMonitorConsumer := func() (err error) {
		var route rtMonitorRoute
		for {
			line, err := stdoutBuf.ReadString('\n')
			if err != nil {
				logging.GetLogger().Errorf("Failed to read 'rt --monitor' output: %s", err)
				return err
			}
			if err := json.Unmarshal([]byte(line), &route); err != nil {
				logging.GetLogger().Error(err)
				continue
			}
			// We currently only support IPV4 routes
			if route.Family != afInetFamily {
				continue
			}
			if route.Operation == "add" || route.Operation == "delete" {
				logging.GetLogger().Debugf("Route add %v", route)
				mapper.routingTableUpdaterChan <- RoutingTableUpdate{action: AddRoute, route: route}
			} else if route.Operation == "delete" {
				logging.GetLogger().Debugf("Route delete %v", route)
				mapper.routingTableUpdaterChan <- RoutingTableUpdate{action: DelRoute, route: route}
			}
		}
		return
	}

	if err := cmd.Start(); err != nil {
		logging.GetLogger().Debug(err)
	}
	go mapper.routingTableUpdater()
	go rtMonitorConsumer()
}
