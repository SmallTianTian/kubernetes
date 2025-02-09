/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package endpoint

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"sync"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	v1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/util/hash"
)

// semanticIgnoreResourceVersion does semantic deep equality checks for objects
// but excludes ResourceVersion of ObjectReference. They are used when comparing
// endpoints in Endpoints and EndpointSlice objects to avoid unnecessary updates
// caused by Pod resourceVersion change.
var semanticIgnoreResourceVersion = conversion.EqualitiesOrDie(
	func(a, b v1.ObjectReference) bool {
		a.ResourceVersion = ""
		b.ResourceVersion = ""
		return a == b
	},
)

// ServiceSelectorCache is a cache of service selectors to avoid high CPU consumption caused by frequent calls to AsSelectorPreValidated (see #73527)
type ServiceSelectorCache struct {
	lock  sync.RWMutex
	cache map[string]labels.Selector
}

// NewServiceSelectorCache init ServiceSelectorCache for both endpoint controller and endpointSlice controller.
func NewServiceSelectorCache() *ServiceSelectorCache {
	return &ServiceSelectorCache{
		cache: map[string]labels.Selector{},
	}
}

// Get return selector and existence in ServiceSelectorCache by key.
func (sc *ServiceSelectorCache) Get(key string) (labels.Selector, bool) {
	sc.lock.RLock()
	selector, ok := sc.cache[key]
	// fine-grained lock improves GetPodServiceMemberships performance(16.5%) than defer measured by BenchmarkGetPodServiceMemberships
	sc.lock.RUnlock()
	return selector, ok
}

// Update can update or add a selector in ServiceSelectorCache while service's selector changed.
func (sc *ServiceSelectorCache) Update(key string, rawSelector map[string]string) labels.Selector {
	sc.lock.Lock()
	defer sc.lock.Unlock()
	selector := labels.Set(rawSelector).AsSelectorPreValidated()
	sc.cache[key] = selector
	return selector
}

// Delete can delete selector which exist in ServiceSelectorCache.
func (sc *ServiceSelectorCache) Delete(key string) {
	sc.lock.Lock()
	defer sc.lock.Unlock()
	delete(sc.cache, key)
}

// GetPodServiceMemberships returns a set of Service keys for Services that have
// a selector matching the given pod.
func (sc *ServiceSelectorCache) GetPodServiceMemberships(serviceLister v1listers.ServiceLister, pod *v1.Pod) (sets.String, error) {
	set := sets.String{}
	services, err := serviceLister.Services(pod.Namespace).List(labels.Everything())
	if err != nil {
		return set, err
	}

	var selector labels.Selector
	for _, service := range services {
		if service.Spec.Selector == nil {
			// if the service has a nil selector this means selectors match nothing, not everything.
			continue
		}
		key, err := controller.KeyFunc(service)
		if err != nil {
			return nil, err
		}
		if v, ok := sc.Get(key); ok {
			selector = v
		} else {
			selector = sc.Update(key, service.Spec.Selector)
		}

		if selector.Matches(labels.Set(pod.Labels)) {
			set.Insert(key)
		}
	}
	return set, nil
}

// PortMapKey is used to uniquely identify groups of endpoint ports.
type PortMapKey string

// NewPortMapKey generates a PortMapKey from endpoint ports.
func NewPortMapKey(endpointPorts []discovery.EndpointPort) PortMapKey {
	sort.Sort(portsInOrder(endpointPorts))
	return PortMapKey(DeepHashObjectToString(endpointPorts))
}

// DeepHashObjectToString creates a unique hash string from a go object.
func DeepHashObjectToString(objectToWrite interface{}) string {
	hasher := md5.New()
	hash.DeepHashObject(hasher, objectToWrite)
	return hex.EncodeToString(hasher.Sum(nil)[0:])
}

// ShouldPodBeInEndpointSlice returns true if a specified pod should be in an EndpointSlice object.
// Terminating pods are only included if includeTerminating is true
func ShouldPodBeInEndpointSlice(pod *v1.Pod, includeTerminating bool) bool {
	if len(pod.Status.PodIP) == 0 && len(pod.Status.PodIPs) == 0 {
		return false
	}

	if !includeTerminating && pod.DeletionTimestamp != nil {
		return false
	}

	if pod.Spec.RestartPolicy == v1.RestartPolicyNever {
		return pod.Status.Phase != v1.PodFailed && pod.Status.Phase != v1.PodSucceeded
	}

	if pod.Spec.RestartPolicy == v1.RestartPolicyOnFailure {
		return pod.Status.Phase != v1.PodSucceeded
	}

	return true
}

// ShouldSetHostname returns true if the Hostname attribute should be set on an
// Endpoints Address or EndpointSlice Endpoint.
func ShouldSetHostname(pod *v1.Pod, svc *v1.Service) bool {
	return len(pod.Spec.Hostname) > 0 && pod.Spec.Subdomain == svc.Name && svc.Namespace == pod.Namespace
}

// podEndpointsChanged returns two boolean values. The first is true if the pod has
// changed in a way that may change existing endpoints. The second value is true if the
// pod has changed in a way that may affect which Services it matches.
func podEndpointsChanged(oldPod, newPod *v1.Pod) (bool, bool) {
	// Check if the pod labels have changed, indicating a possible
	// change in the service membership
	labelsChanged := false
	if !reflect.DeepEqual(newPod.Labels, oldPod.Labels) ||
		!hostNameAndDomainAreEqual(newPod, oldPod) {
		labelsChanged = true
	}

	// If the pod's deletion timestamp is set, remove endpoint from ready address.
	if newPod.DeletionTimestamp != oldPod.DeletionTimestamp {
		return true, labelsChanged
	}
	// If the pod's readiness has changed, the associated endpoint address
	// will move from the unready endpoints set to the ready endpoints.
	// So for the purposes of an endpoint, a readiness change on a pod
	// means we have a changed pod.
	if podutil.IsPodReady(oldPod) != podutil.IsPodReady(newPod) {
		return true, labelsChanged
	}

	// Check if the pod IPs have changed
	if len(oldPod.Status.PodIPs) != len(newPod.Status.PodIPs) {
		return true, labelsChanged
	}
	for i := range oldPod.Status.PodIPs {
		if oldPod.Status.PodIPs[i].IP != newPod.Status.PodIPs[i].IP {
			return true, labelsChanged
		}
	}

	// Endpoints may also reference a pod's Name, Namespace, UID, and NodeName, but
	// the first three are immutable, and NodeName is immutable once initially set,
	// which happens before the pod gets an IP.

	return false, labelsChanged
}

// GetServicesToUpdateOnPodChange returns a set of Service keys for Services
// that have potentially been affected by a change to this pod.
func GetServicesToUpdateOnPodChange(serviceLister v1listers.ServiceLister, selectorCache *ServiceSelectorCache, old, cur interface{}) sets.String {
	newPod := cur.(*v1.Pod)
	oldPod := old.(*v1.Pod)
	if newPod.ResourceVersion == oldPod.ResourceVersion {
		// Periodic resync will send update events for all known pods.
		// Two different versions of the same pod will always have different RVs
		return sets.String{}
	}

	podChanged, labelsChanged := podEndpointsChanged(oldPod, newPod)

	// If both the pod and labels are unchanged, no update is needed
	if !podChanged && !labelsChanged {
		return sets.String{}
	}

	services, err := selectorCache.GetPodServiceMemberships(serviceLister, newPod)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to get pod %s/%s's service memberships: %v", newPod.Namespace, newPod.Name, err))
		return sets.String{}
	}

	if labelsChanged {
		oldServices, err := selectorCache.GetPodServiceMemberships(serviceLister, oldPod)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("Unable to get pod %s/%s's service memberships: %v", newPod.Namespace, newPod.Name, err))
		}
		services = determineNeededServiceUpdates(oldServices, services, podChanged)
	}

	return services
}

// GetPodFromDeleteAction returns a pointer to a pod if one can be derived from
// obj (could be a *v1.Pod, or a DeletionFinalStateUnknown marker item).
func GetPodFromDeleteAction(obj interface{}) *v1.Pod {
	if pod, ok := obj.(*v1.Pod); ok {
		// Enqueue all the services that the pod used to be a member of.
		// This is the same thing we do when we add a pod.
		return pod
	}
	// If we reached here it means the pod was deleted but its final state is unrecorded.
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
		return nil
	}
	pod, ok := tombstone.Obj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("Tombstone contained object that is not a Pod: %#v", obj))
		return nil
	}
	return pod
}

func hostNameAndDomainAreEqual(pod1, pod2 *v1.Pod) bool {
	return pod1.Spec.Hostname == pod2.Spec.Hostname &&
		pod1.Spec.Subdomain == pod2.Spec.Subdomain
}

func determineNeededServiceUpdates(oldServices, services sets.String, podChanged bool) sets.String {
	if podChanged {
		// if the labels and pod changed, all services need to be updated
		services = services.Union(oldServices)
	} else {
		// if only the labels changed, services not common to both the new
		// and old service set (the disjuntive union) need to be updated
		services = services.Difference(oldServices).Union(oldServices.Difference(services))
	}
	return services
}

// portsInOrder helps sort endpoint ports in a consistent way for hashing.
type portsInOrder []discovery.EndpointPort

func (sl portsInOrder) Len() int      { return len(sl) }
func (sl portsInOrder) Swap(i, j int) { sl[i], sl[j] = sl[j], sl[i] }
func (sl portsInOrder) Less(i, j int) bool {
	h1 := DeepHashObjectToString(sl[i])
	h2 := DeepHashObjectToString(sl[j])
	return h1 < h2
}

// endpointsEqualBeyondHash returns true if endpoints have equal attributes
// but excludes equality checks that would have already been covered with
// endpoint hashing (see hashEndpoint func for more info) and ignores difference
// in ResourceVersion of TargetRef.
func EndpointsEqualBeyondHash(ep1, ep2 *discovery.Endpoint) bool {
	if stringPtrChanged(ep1.NodeName, ep1.NodeName) {
		return false
	}

	if stringPtrChanged(ep1.Zone, ep1.Zone) {
		return false
	}

	if boolPtrChanged(ep1.Conditions.Ready, ep2.Conditions.Ready) {
		return false
	}

	// Serving and Terminating will only be set when the EndpointSliceTerminatingCondition feature is on.
	// Ignore their difference if the expected or actual value is nil, which means the feature enablement is changed.
	// Otherwise all EndpointSlices in the system would be updated on the first controller-manager restart even without
	// actual changes, leading to delay in processing legitimate updates.
	// Its value will be set to the expected one when there is an actual change triggering update of this EndpointSlice.
	if ep1.Conditions.Serving != nil && ep2.Conditions.Serving != nil && *ep1.Conditions.Serving != *ep2.Conditions.Serving {
		return false
	}

	if ep1.Conditions.Terminating != nil && ep2.Conditions.Terminating != nil && *ep1.Conditions.Terminating != *ep2.Conditions.Terminating {
		return false
	}

	if !semanticIgnoreResourceVersion.DeepEqual(ep1.TargetRef, ep2.TargetRef) {
		return false
	}

	return true
}

// boolPtrChanged returns true if a set of bool pointers have different values.
func boolPtrChanged(ptr1, ptr2 *bool) bool {
	if (ptr1 == nil) != (ptr2 == nil) {
		return true
	}
	if ptr1 != nil && ptr2 != nil && *ptr1 != *ptr2 {
		return true
	}
	return false
}

// stringPtrChanged returns true if a set of string pointers have different values.
func stringPtrChanged(ptr1, ptr2 *string) bool {
	if (ptr1 == nil) != (ptr2 == nil) {
		return true
	}
	if ptr1 != nil && ptr2 != nil && *ptr1 != *ptr2 {
		return true
	}
	return false
}

// EndpointSubsetsEqualIgnoreResourceVersion returns true if EndpointSubsets
// have equal attributes but excludes ResourceVersion of Pod.
func EndpointSubsetsEqualIgnoreResourceVersion(subsets1, subsets2 []v1.EndpointSubset) bool {
	return semanticIgnoreResourceVersion.DeepEqual(subsets1, subsets2)
}
