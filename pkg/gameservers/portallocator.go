// Copyright 2018 Google Inc. All Rights Reserved.
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

package gameservers

import (
	"sync"

	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	"agones.dev/agones/pkg/client/informers/externalversions"
	listerv1alpha1 "agones.dev/agones/pkg/client/listers/stable/v1alpha1"
	"agones.dev/agones/pkg/util/runtime"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	corelisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// ErrPortNotFound is returns when a port is unable to be allocated
var ErrPortNotFound = errors.New("Unable to allocate a port")

// A set of port allocations for a node
type portAllocation map[int32]bool

// PortAllocator manages the dynamic port
// allocation strategy. Only use exposed methods to ensure
// appropriate locking is taken.
// The PortAllocator does not currently support mixing static portAllocations (or any pods with defined HostPort)
// within the dynamic port range other than the ones it coordinates.
type PortAllocator struct {
	logger             *logrus.Entry
	mutex              sync.RWMutex
	portAllocations    []portAllocation
	gameServerRegistry map[types.UID]bool
	nodeRegistry       map[types.UID]bool
	minPort            int32
	maxPort            int32
	gameServerSynced   cache.InformerSynced
	gameServerLister   listerv1alpha1.GameServerLister
	gameServerInformer cache.SharedIndexInformer
	nodeSynced         cache.InformerSynced
	nodeLister         corelisterv1.NodeLister
	nodeInformer       cache.SharedIndexInformer
}

// NewPortAllocator returns a new dynamic port
// allocator. minPort and maxPort are the top and bottom portAllocations that can be allocated in the range for
// the game servers
func NewPortAllocator(minPort, maxPort int32,
	kubeInformerFactory informers.SharedInformerFactory,
	agonesInformerFactory externalversions.SharedInformerFactory) *PortAllocator {

	v1 := kubeInformerFactory.Core().V1()
	nodes := v1.Nodes()
	gameServers := agonesInformerFactory.Stable().V1alpha1().GameServers()

	pa := &PortAllocator{
		mutex:              sync.RWMutex{},
		minPort:            minPort,
		maxPort:            maxPort,
		gameServerRegistry: map[types.UID]bool{},
		nodeRegistry:       map[types.UID]bool{},
		gameServerSynced:   gameServers.Informer().HasSynced,
		gameServerLister:   gameServers.Lister(),
		gameServerInformer: gameServers.Informer(),
		nodeLister:         nodes.Lister(),
		nodeInformer:       nodes.Informer(),
		nodeSynced:         nodes.Informer().HasSynced,
	}
	pa.logger = runtime.NewLoggerWithType(pa)

	pa.gameServerInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: pa.syncDeleteGameServer,
	})

	// Experimental support for node adding/removal
	pa.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: pa.syncAddNode,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode := oldObj.(*corev1.Node)
			newNode := newObj.(*corev1.Node)
			if oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable {
				err := pa.syncPortAllocations()
				if err != nil {
					err = errors.Wrap(err, "error resetting ports on node update")
					runtime.HandleError(pa.logger.WithField("node", newNode), err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			err := pa.syncPortAllocations()
			if err != nil {
				err = errors.Wrap(err, "error on node deletion")
				runtime.HandleError(pa.logger.WithField("node", obj), err)
			}
		},
	})

	pa.logger.WithField("minPort", minPort).WithField("maxPort", maxPort).Info("Starting")
	return pa
}

// Run sets up the current state of port allocations and
// starts tracking Pod and Node changes (non blocking)
func (pa *PortAllocator) Run(stop <-chan struct{}) error {
	pa.logger.Info("Running")

	if !cache.WaitForCacheSync(stop, pa.gameServerSynced, pa.nodeSynced) {
		return errors.New("failed to wait for caches to sync")
	}

	return pa.syncPortAllocations()
}

// Allocate assigns a port to the GameServer and returns it.
// Return ErrPortNotFound if no port is allocatable
func (pa *PortAllocator) Allocate(gs *v1alpha1.GameServer) (*v1alpha1.GameServer, error) {
	if gs.Spec.PortPolicy != v1alpha1.Dynamic {
		return gs, errors.Errorf("Port policy of %s is not supported for port allocation", gs.Spec.PortPolicy)
	}

	pa.mutex.Lock()
	defer pa.mutex.Unlock()
	for _, n := range pa.portAllocations {
		for p, taken := range n {
			if !taken {
				n[p] = true
				pa.gameServerRegistry[gs.ObjectMeta.UID] = true
				gs.Spec.HostPort = p
				return gs, nil
			}
		}
	}
	return gs, ErrPortNotFound
}

// DeAllocate marks the given port as no longer allocated
func (pa *PortAllocator) DeAllocate(gs *v1alpha1.GameServer) {
	if gs.Spec.PortPolicy != v1alpha1.Dynamic {
		return
	}
	if gs.Spec.HostPort < pa.minPort || gs.Spec.HostPort > pa.maxPort {
		return
	}
	// skip if it wasn't previously allocated
	if _, ok := pa.gameServerRegistry[gs.ObjectMeta.UID]; !ok {
		pa.logger.WithField("gs", gs.ObjectMeta.Name).
			Info("Did not allocate this GameServer. Ignoring for DeAllocation")
		return
	}
	pa.mutex.Lock()
	defer pa.mutex.Unlock()
	pa.portAllocations = setPortAllocation(gs.Spec.HostPort, pa.portAllocations, false)
	delete(pa.gameServerRegistry, gs.ObjectMeta.UID)
}

// syncAddNode adds another node port section
// to the available ports
func (pa *PortAllocator) syncAddNode(obj interface{}) {
	node := obj.(*corev1.Node)
	// if we're already added this node, don't do it again
	if _, ok := pa.nodeRegistry[node.ObjectMeta.UID]; ok {
		pa.logger.WithField("node", node.ObjectMeta.Name).Info("Already added node to port allocations. Skipping")
		return
	}

	pa.logger.WithField("node", node.ObjectMeta.Name).Info("Adding Node to port allocations")
	pa.mutex.Lock()
	defer pa.mutex.Unlock()

	ports := portAllocation{}
	for i := pa.minPort; i <= pa.maxPort; i++ {
		ports[i] = false
	}

	pa.portAllocations = append(pa.portAllocations, ports)
	pa.nodeRegistry[node.ObjectMeta.UID] = true
}

// syncDeleteGameServer when a GameServer Pod is deleted
// make the HostPort available
func (pa *PortAllocator) syncDeleteGameServer(object interface{}) {
	if gs, ok := object.(*v1alpha1.GameServer); ok {
		pa.logger.WithField("gs", gs).Info("syncing deleted GameServer")
		pa.DeAllocate(gs)
	}
}

// syncPortAllocations syncs the pod, node and gameserver caches then
// traverses all Nodes in the cluster and all looks at GameServers
// and Terminating Pods values make sure those
// portAllocations are marked as taken.
// Locks the mutex while doing this.
// This is basically a stop the world Garbage Collection on port allocations.
func (pa *PortAllocator) syncPortAllocations() error {
	pa.mutex.Lock()
	defer pa.mutex.Unlock()

	pa.logger.Info("Resetting Port Allocation")

	nodes, err := pa.nodeLister.List(labels.Everything())
	if err != nil {
		return errors.Wrap(err, "error listing all nodes")
	}

	// setup blank port values
	nodePorts, nodeRegistry := pa.nodePortAllocation(nodes)

	gameservers, err := pa.gameServerLister.List(labels.Everything())
	if err != nil {
		return errors.Wrapf(err, "error listing all GameServers")
	}

	gsRegistry := map[types.UID]bool{}

	// place to put GameServer port allocations that are not ready yet/after the ready state
	var nonReadyNodesPorts []int32
	// Check GameServers as well, as some
	for _, gs := range gameservers {
		if gs.Spec.PortPolicy == v1alpha1.Dynamic {
			gsRegistry[gs.ObjectMeta.UID] = true

			// if the node doesn't exist, it's likely unscheduled
			_, ok := nodePorts[gs.Status.NodeName]
			if gs.Status.NodeName != "" && ok {
				nodePorts[gs.Status.NodeName][gs.Status.Port] = true
			} else if gs.Spec.HostPort != 0 {
				nonReadyNodesPorts = append(nonReadyNodesPorts, gs.Spec.HostPort)
			}
		}
	}

	// this gives us back an ordered node list.
	allocations := make([]portAllocation, len(nodePorts))
	i := 0
	for _, np := range nodePorts {
		allocations[i] = np
		i++
	}

	// close off the port on the first node you find
	// we actually don't mind what node it is, since we only care
	// that there is a port open *somewhere* as the default scheduler
	// will re-route for us based on HostPort allocation
	for _, p := range nonReadyNodesPorts {
		allocations = setPortAllocation(p, allocations, true)
	}

	pa.portAllocations = allocations
	pa.gameServerRegistry = gsRegistry
	pa.nodeRegistry = nodeRegistry

	return nil
}

// nodePortAllocation returns a map of port allocations all set to being available
// with a map key for each node, as well as the node registry record (since we're already looping)
func (pa *PortAllocator) nodePortAllocation(nodes []*corev1.Node) (map[string]portAllocation, map[types.UID]bool) {
	nodePorts := map[string]portAllocation{}
	nodeRegistry := map[types.UID]bool{}

	for _, n := range nodes {
		nodeRegistry[n.ObjectMeta.UID] = true

		// ignore unschedulable nodes
		if !n.Spec.Unschedulable {
			nodePorts[n.Name] = portAllocation{}
			for i := pa.minPort; i <= pa.maxPort; i++ {
				nodePorts[n.Name][i] = false
			}
		}
	}

	return nodePorts, nodeRegistry
}

// setPortAllocation takes a port from an all
func setPortAllocation(port int32, allocations []portAllocation, taken bool) []portAllocation {
	for _, np := range allocations {
		if np[port] != taken {
			np[port] = taken
			break
		}
	}
	return allocations
}
