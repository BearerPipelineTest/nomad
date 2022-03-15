package nomad

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/client/serviceregistration"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/nomad/structs"
)

type ServiceRegistrationHandler struct {
	log hclog.Logger
	cfg *ServiceRegistrationHandlerCfg

	// registrationEnabled tracks whether this handler is enabled for
	// registrations. This is needed as it's possible a client has its config
	// changed whilst allocations using this provider are running on it. In
	// this situation we need to be able to deregister services, but disallow
	// registering new ones.
	registrationEnabled bool

	minBackoffInterval time.Duration
	maxBackoffInterval time.Duration
	maxBackoffDuration time.Duration
}

// ServiceRegistrationHandlerCfg holds critical information used during the
// normal process of the ServiceRegistrationHandler. It is used to keep the
// NewServiceRegistrationHandler function signature small and easy to modify.
type ServiceRegistrationHandlerCfg struct {

	// Enabled tracks whether this client feature is enabled.
	Enabled bool

	// Datacenter, NodeID, and Region are all properties of the Nomad client
	// and are used to perform RPC requests.
	Datacenter string
	NodeID     string
	Region     string

	// NodeSecret is the secret ID of the node and is used to authenticate RPC
	// requests.
	NodeSecret string

	// RPCFn is the client RPC function which is used to perform client to
	// server service registration RPC calls.
	RPCFn func(method string, args, resp interface{}) error
}

// NewServiceRegistrationHandler returns a ready to use
// ServiceRegistrationHandler which implements the serviceregistration.Handler
// interface.
func NewServiceRegistrationHandler(
	log hclog.Logger, cfg *ServiceRegistrationHandlerCfg) serviceregistration.Handler {
	return &ServiceRegistrationHandler{
		cfg:                 cfg,
		log:                 log.Named("service_registration.nomad"),
		registrationEnabled: cfg.Enabled,
		minBackoffInterval:  time.Second,
		maxBackoffInterval:  time.Minute,
		maxBackoffDuration:  time.Hour * 24,
	}
}

func (s *ServiceRegistrationHandler) RegisterWorkload(workload *serviceregistration.WorkloadServices) error {

	// Check whether we are enabled or not first.
	if !s.registrationEnabled {
		return errors.New("service registration provider not enabled")
	}

	// Collect all errors generating service registrations.
	var mErr multierror.Error

	registrations := make([]*structs.ServiceRegistration, len(workload.Services))

	// Iteration the services and generated a hydrated registration object for
	// each. All services are part of a single allocation, therefore we cannot
	// have one failure without all becoming a failure.
	for i, serviceSpec := range workload.Services {
		serviceRegistration, err := s.generateNomadServiceRegistration(serviceSpec, workload)
		if err != nil {
			mErr.Errors = append(mErr.Errors, err)
		} else if mErr.ErrorOrNil() == nil {
			registrations[i] = serviceRegistration
		}
	}

	// If we generated any errors, return this to the caller.
	if err := mErr.ErrorOrNil(); err != nil {
		return err
	}

	args := structs.ServiceRegistrationUpsertRequest{
		Services: registrations,
		WriteRequest: structs.WriteRequest{
			Region:    s.cfg.Region,
			AuthToken: s.cfg.NodeSecret,
		},
	}

	var resp structs.ServiceRegistrationUpsertResponse

	return s.retryFunc(func() error {
		return s.cfg.RPCFn(structs.ServiceRegistrationUpsertRPCMethod, &args, &resp)
	}, structs.ServiceRegistrationUpsertRPCMethod)
}

// RemoveWorkload iterates the services and removes them from the service
// registration state.
//
// This function works regardless of whether the client has this feature
// enabled. This covers situations where the feature is disabled, yet still has
// allocations which, when stopped need their registrations removed.
func (s *ServiceRegistrationHandler) RemoveWorkload(workload *serviceregistration.WorkloadServices) {
	for _, serviceSpec := range workload.Services {
		go s.removeWorkload(workload, serviceSpec)
	}
}

func (s *ServiceRegistrationHandler) removeWorkload(
	workload *serviceregistration.WorkloadServices, serviceSpec *structs.Service) {

	// Generate the consistent ID for this service, so we know what to remove.
	id := serviceregistration.MakeAllocServiceID(workload.AllocID, workload.Name(), serviceSpec)

	deleteArgs := structs.ServiceRegistrationDeleteByIDRequest{
		ID: id,
		WriteRequest: structs.WriteRequest{
			Region:    s.cfg.Region,
			Namespace: workload.Namespace,
			AuthToken: s.cfg.NodeSecret,
		},
	}

	var deleteResp structs.ServiceRegistrationDeleteByIDResponse

	// Create our function that will be retried.
	f := func() error {
		err := s.cfg.RPCFn(structs.ServiceRegistrationDeleteByIDRPCMethod, &deleteArgs, &deleteResp)
		if err == nil {
			return nil
		}

		// The Nomad API exposes service registration deletion to handle
		// orphaned service registrations. In the event a service is removed
		// accidentally that is still running, we will hit this error when we
		// eventually want to remove it. We therefore want to handle this,
		// while ensuring the operator can see.
		if strings.Contains(err.Error(), "service registration not found") {
			s.log.Info("attempted to delete non-existent service registration",
				"service_id", id, "namespace", workload.Namespace)
			return nil
		}

		return err
	}

	// Perform our function retry, logging any error with enough detail for
	// operators to debug properly.
	if err := s.retryFunc(f, structs.ServiceRegistrationDeleteByIDRPCMethod); err != nil {
		s.log.Error("failed to delete service registration",
			"error", err, "service_id", id, "namespace", workload.Namespace)
	}
}

func (s *ServiceRegistrationHandler) UpdateWorkload(old, new *serviceregistration.WorkloadServices) error {

	// Overwrite the workload with the deduplicated versions.
	old, new = s.dedupUpdatedWorkload(old, new)

	// Use the register error as an update protection and only ever deregister
	// when this has completed successfully. In the event of an error, we can
	// return this to the caller stack without modifying state in a weird half
	// manner.
	if len(new.Services) > 0 {
		if err := s.RegisterWorkload(new); err != nil {
			return err
		}
	}

	if len(old.Services) > 0 {
		s.RemoveWorkload(old)
	}

	return nil
}

// dedupUpdatedWorkload works through the request old and new workload to
// return a deduplicated set of services.
//
// This is within its own function to make testing easier.
func (s *ServiceRegistrationHandler) dedupUpdatedWorkload(
	oldWork, newWork *serviceregistration.WorkloadServices) (
	*serviceregistration.WorkloadServices, *serviceregistration.WorkloadServices) {

	// Create copies of the old and new workload services. These specifically
	// ignore the services array so this can be populated as the function
	// decides what is needed.
	oldCopy := oldWork.Copy()
	oldCopy.Services = make([]*structs.Service, 0)

	newCopy := newWork.Copy()
	newCopy.Services = make([]*structs.Service, 0)

	// Generate and populate a mapping of the new service registration IDs.
	newIDs := make(map[string]*structs.Service, len(newWork.Services))

	for _, s := range newWork.Services {
		newIDs[serviceregistration.MakeAllocServiceID(newWork.AllocID, newWork.Name(), s)] = s
	}

	// Iterate through the old services in order to identify whether they can
	// be modified solely via upsert, or whether they need to be deleted.
	for _, oldService := range oldWork.Services {

		// Generate the service ID of the old service. If this is not found
		// within the new mapping then we need to remove it.
		oldID := serviceregistration.MakeAllocServiceID(oldWork.AllocID, oldWork.Name(), oldService)
		newSvc, ok := newIDs[oldID]
		if !ok {
			oldCopy.Services = append(oldCopy.Services, oldService)
			continue
		}

		// Add the new service into the array for upserting and remove its
		// entry for the map. Doing it here is efficient as we are already
		// inside a loop.
		//
		// There isn't much point in hashing the old/new services as we would
		// still need to ensure the service has previously been registered
		// before discarding it from future RPC calls. The Nomad state handles
		// performing the diff gracefully, therefore this will still be a
		// single RPC.
		newCopy.Services = append(newCopy.Services, newSvc)
		delete(newIDs, oldID)
	}

	// Iterate the remaining new IDs to add them to the registration array. It
	// catches any that didn't get added via the previous loop.
	for _, newSvc := range newIDs {
		newCopy.Services = append(newCopy.Services, newSvc)
	}

	return oldCopy, newCopy
}

// AllocRegistrations is currently a noop implementation.
func (s *ServiceRegistrationHandler) AllocRegistrations(_ string) (*serviceregistration.AllocRegistration, error) {
	return nil, nil
}

// UpdateTTL is currently a noop implementation.
func (s *ServiceRegistrationHandler) UpdateTTL(_, _, _, _ string) error {
	return nil
}

// retryFunc handles performing retries of a passed function with backoff. The
// initial function will be triggered immediately; any error returned by this
// function should be considered terminal. It is designed to handle RPC calls
// only, with the function wrapper adding flexibility.
func (s *ServiceRegistrationHandler) retryFunc(fn func() error, method string) error {

	ctx, cancel := context.WithTimeout(context.TODO(), s.maxBackoffDuration)
	defer cancel()

	// Store the error outside the loop, so we can always return the last error
	// recorded from the RPC.
	var err error

	// Copy the backoff, so we can make local changes without altering the
	// stored base value.
	backoff := s.minBackoffInterval

	// Create a new timer, initially set to zero so that it fires straight
	// away.
	t, stop := helper.NewSafeTimer(0)
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return err
		case <-t.C:
		}

		// Execute the function.
		if err = fn(); err == nil {
			break
		}

		if backoff < s.maxBackoffInterval {
			backoff = backoff * 2
			if backoff > s.maxBackoffInterval {
				backoff = s.maxBackoffInterval
			}
		}

		// Log that the RPC failed along with useful context and reset the
		// timer using the new backoff value.
		s.log.Debug("service registration RPC failed",
			"method", method, "retry", backoff, "error", err)
		t.Reset(backoff)
	}

	return nil
}

// generateNomadServiceRegistration is a helper to build the Nomad specific
// registration object on a per-service basis.
func (s *ServiceRegistrationHandler) generateNomadServiceRegistration(
	serviceSpec *structs.Service, workload *serviceregistration.WorkloadServices) (*structs.ServiceRegistration, error) {

	// Service address modes default to auto.
	addrMode := serviceSpec.AddressMode
	if addrMode == "" {
		addrMode = structs.AddressModeAuto
	}

	// Determine the address to advertise based on the mode.
	ip, port, err := serviceregistration.GetAddress(
		addrMode, serviceSpec.PortLabel, workload.Networks,
		workload.DriverNetwork, workload.Ports, workload.NetworkStatus)
	if err != nil {
		return nil, fmt.Errorf("unable to get address for service %q: %v", serviceSpec.Name, err)
	}

	// Build the tags to use for this registration which is a result of whether
	// this is a canary, or not.
	var tags []string

	if workload.Canary && len(serviceSpec.CanaryTags) > 0 {
		tags = make([]string, len(serviceSpec.CanaryTags))
		copy(tags, serviceSpec.CanaryTags)
	} else {
		tags = make([]string, len(serviceSpec.Tags))
		copy(tags, serviceSpec.Tags)
	}

	return &structs.ServiceRegistration{
		ID:          serviceregistration.MakeAllocServiceID(workload.AllocID, workload.Name(), serviceSpec),
		ServiceName: serviceSpec.Name,
		NodeID:      s.cfg.NodeID,
		JobID:       workload.JobID,
		AllocID:     workload.AllocID,
		Namespace:   workload.Namespace,
		Datacenter:  s.cfg.Datacenter,
		Tags:        tags,
		Address:     ip,
		Port:        port,
	}, nil
}
