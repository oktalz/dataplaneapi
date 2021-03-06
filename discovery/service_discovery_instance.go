// Copyright 2019 HAProxy Technologies
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package discovery

import (
	"github.com/haproxytech/client-native/v2/configuration"
	"github.com/haproxytech/dataplaneapi/haproxy"
)

// ServiceInstance specifies the needed information required from the service to provide for the ServiceDiscoveryInstance.
type ServiceInstance interface {
	GetName() string
	GetBackendName() string
	Changed() bool
	GetServers() []configuration.ServiceServer
}

type confService struct {
	confService *configuration.Service
	deleted     bool
}

type discoveryInstanceParams struct {
	Whitelist       []string
	Blacklist       []string
	ServerSlotsBase int
	SlotsGrowthType string
	SlotsIncrement  int
}

// ServiceDiscoveryInstance manages and updates all services of a single service discovery.
type ServiceDiscoveryInstance struct {
	services      map[string]*confService
	client        *configuration.Client
	reloadAgent   haproxy.IReloadAgent
	params        discoveryInstanceParams
	transactionID string
}

// NewServiceDiscoveryInstance creates a new ServiceDiscoveryInstance.
func NewServiceDiscoveryInstance(client *configuration.Client, reloadAgent haproxy.IReloadAgent, params discoveryInstanceParams) *ServiceDiscoveryInstance {
	return &ServiceDiscoveryInstance{
		client:      client,
		reloadAgent: reloadAgent,
		params:      params,
		services:    make(map[string]*confService),
	}
}

// UpdateParams updates the scaling params for each service associated with the service discovery.
func (s *ServiceDiscoveryInstance) UpdateParams(params discoveryInstanceParams) error {
	s.params = params
	for _, se := range s.services {
		err := se.confService.UpdateScalingParams(configuration.ScalingParams{
			BaseSlots:       s.params.ServerSlotsBase,
			SlotsGrowthType: s.params.SlotsGrowthType,
			SlotsIncrement:  s.params.SlotsIncrement,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// UpdateServices updates each service and persists the changes inside a single transaction.
func (s *ServiceDiscoveryInstance) UpdateServices(services []ServiceInstance) error {
	err := s.startTransaction()
	if err != nil {
		return err
	}
	reload := false
	s.markForDeletion()
	for _, service := range services {
		if s.serviceNotTracked(service.GetName()) {
			continue
		}
		if !service.Changed() {
			s.services[service.GetName()].deleted = false
			continue
		}
		r, err := s.initService(service)
		if err != nil {
			s.deleteTransaction()
			return err
		}
		reload = reload || r
		se := s.services[service.GetName()]
		r, err = se.confService.Update(service.GetServers())
		if err != nil {
			s.deleteTransaction()
			return err
		}
		reload = reload || r
	}
	r := s.removeDeleted()
	reload = reload || r
	if reload {
		if err := s.commitTransaction(); err != nil {
			return err
		}
		s.reloadAgent.Reload()
		return nil
	}
	s.deleteTransaction()
	return nil
}

func (s *ServiceDiscoveryInstance) startTransaction() error {
	version, err := s.client.GetVersion("")
	if err != nil {
		return err
	}
	transaction, err := s.client.StartTransaction(version)
	if err != nil {
		return err
	}
	s.transactionID = transaction.ID
	return nil
}

func (s *ServiceDiscoveryInstance) markForDeletion() {
	for id := range s.services {
		s.services[id].deleted = true
	}
}

func (s *ServiceDiscoveryInstance) serviceNotTracked(service string) bool {
	if len(s.params.Whitelist) > 0 {
		for _, se := range s.params.Whitelist {
			if se == service {
				return false
			}
		}
		return true
	}
	for _, se := range s.params.Blacklist {
		if se == service {
			return true
		}
	}
	return false
}

func (s *ServiceDiscoveryInstance) initService(service ServiceInstance) (bool, error) {
	if se, ok := s.services[service.GetName()]; ok {
		se.confService.SetTransactionID(s.transactionID)
		se.deleted = false
		return false, nil
	}
	se, err := s.client.NewService(service.GetBackendName(), configuration.ScalingParams{
		BaseSlots:       s.params.ServerSlotsBase,
		SlotsGrowthType: s.params.SlotsGrowthType,
		SlotsIncrement:  s.params.SlotsIncrement,
	})
	if err != nil {
		return false, err
	}
	reload, err := se.Init(s.transactionID)
	if err != nil {
		return false, err
	}
	s.services[service.GetName()] = &confService{
		confService: se,
		deleted:     false,
	}
	return reload, nil
}

func (s *ServiceDiscoveryInstance) removeDeleted() bool {
	reload := false
	for service := range s.services {
		if s.services[service].deleted {
			s.services[service].confService.SetTransactionID(s.transactionID)
			err := s.services[service].confService.Delete()
			if err == nil {
				reload = true
				delete(s.services, service)
			}
		}
	}
	return reload
}

func (s *ServiceDiscoveryInstance) deleteTransaction() {
	//nolint
	s.client.DeleteTransaction(s.transactionID)
	s.transactionID = ""
}

func (s *ServiceDiscoveryInstance) commitTransaction() error {
	_, err := s.client.CommitTransaction(s.transactionID)
	s.transactionID = ""
	return err
}
