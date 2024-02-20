package ocp_dnsnameresolver

import (
	"fmt"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin"

	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	ocpnetworkclient "github.com/openshift/client-go/network/clientset/versioned"
	ocpnetworkclientv1alpha1 "github.com/openshift/client-go/network/clientset/versioned/typed/network/v1alpha1"
	ocpnetworkinformer "github.com/openshift/client-go/network/informers/externalversions"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// namespaceDNSInfo is used to store information regarding a particular DNS name.
// The map stores the namespaces where a DNSNameResolver object corresponding to
// the DNS name is created.
// key: namespace, value: object name.
type namespaceDNSInfo map[string]string

// OCPDNSNameResolver is a plugin that looks up responses from other plugins
// and updates the status of DNSNameResolver objects.
type OCPDNSNameResolver struct {
	Next plugin.Handler

	// configurable fields.
	namespaces       map[string]struct{}
	minimumTTL       int32
	failureThreshold int32

	// maps for storing regular and wildcard DNS name info.
	// data mapping: DNS name --> Namespace --> DNSNameResolver object name.
	// key: DNS name, value: namespaceDNSInfo map containing information
	// about the namespaces where a DNSNameResolver object corresponding to
	// the DNS name is created.
	regularDNSInfo  map[string]namespaceDNSInfo
	wildcardDNSInfo map[string]namespaceDNSInfo
	regularMapLock  sync.Mutex
	wildcardMapLock sync.Mutex

	// client and informer for handling DNSNameResolver objects.
	ocpNetworkClient        ocpnetworkclientv1alpha1.NetworkV1alpha1Interface
	dnsNameResolverInformer cache.SharedIndexInformer
	stopCh                  chan struct{}
	stopLock                sync.Mutex
	shutdown                bool
}

// New returns an initialized OCPDNSNameResolver with default settings.
func New() *OCPDNSNameResolver {
	return &OCPDNSNameResolver{
		regularDNSInfo:   make(map[string]namespaceDNSInfo),
		wildcardDNSInfo:  make(map[string]namespaceDNSInfo),
		namespaces:       make(map[string]struct{}),
		minimumTTL:       defaultMinTTL,
		failureThreshold: defaultFailureThreshold,
	}
}

const (
	// defaultResyncPeriod gives the resync period used for creating the DNSNameResolver informer.
	defaultResyncPeriod = 24 * time.Hour
	// defaultMinTTL will be used when minTTL is not explicitly configured.
	defaultMinTTL int32 = 5
	// defaultFailureThreshold will be used when failureThreshold is not explicitly configured.
	defaultFailureThreshold int32 = 5
)

// initInformer initializes the DNSNameResolver informer.
func (resolver *OCPDNSNameResolver) initInformer(networkClient ocpnetworkclient.Interface, send func(*ocpnetworkapiv1alpha1.DNSNameResolver)) (err error) {
	// Get the client for version v1alpha1 for DNSNameResolver objects.
	resolver.ocpNetworkClient = networkClient.NetworkV1alpha1()

	// Create the DNSNameResolver informer.
	resolver.dnsNameResolverInformer = ocpnetworkinformer.NewSharedInformerFactory(networkClient, defaultResyncPeriod).Network().V1alpha1().DNSNameResolvers().Informer()

	// Add the event handlers for Add, Delete and Update events.
	resolver.dnsNameResolverInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		// Add event.
		AddFunc: func(obj interface{}) {
			// Get the DNSNameResolver object.
			resolverObj, ok := obj.(*ocpnetworkapiv1alpha1.DNSNameResolver)
			if !ok {
				log.Infof("object not of type DNSNameResolver: %v", obj)
				return
			}

			// Check if namespace is configured or not.
			if !resolver.configuredNamespace(resolverObj.Namespace) {
				return
			}

			dnsName := string(resolverObj.Spec.Name)
			// Check if the DNS name is wildcard or regular.
			if isWildcard(dnsName) {
				// If the DNS name is wildcard, add the details of the DNSNameResolver
				// object to the wildcardDNSInfo map.
				resolver.wildcardMapLock.Lock()
				dnsInfoMap, exists := resolver.wildcardDNSInfo[dnsName]
				// If details of DNS name and the DNSNameResolver objects already exist
				// then check if the existing information match with the current one.
				// Otherwise, don't proceed.
				if exists && dnsInfoMap[resolverObj.Namespace] != resolverObj.Name {
					resolver.wildcardMapLock.Unlock()
					return
				}
				if !exists {
					dnsInfoMap = make(namespaceDNSInfo)
				}
				dnsInfoMap[resolverObj.Namespace] = resolverObj.Name
				resolver.wildcardDNSInfo[dnsName] = dnsInfoMap
				resolver.wildcardMapLock.Unlock()
			} else {
				// If the DNS name is regular, add the details of the DNSNameResolver
				// object to the regularDNSInfo map.
				resolver.regularMapLock.Lock()
				dnsInfoMap, exists := resolver.regularDNSInfo[dnsName]
				// If details of DNS name and the DNSNameResolver objects already exist
				// then check if the existing information match with the current one.
				// Otherwise, don't proceed.
				if exists && dnsInfoMap[resolverObj.Namespace] != resolverObj.Name {
					resolver.regularMapLock.Unlock()
					return
				}
				if !exists {
					dnsInfoMap = make(namespaceDNSInfo)
				}
				dnsInfoMap[resolverObj.Namespace] = resolverObj.Name
				resolver.regularDNSInfo[dnsName] = dnsInfoMap
				resolver.regularMapLock.Unlock()
			}

			// Used only in unit tests.
			if send != nil {
				send(resolverObj)
			}
		},
		// Delete event.
		DeleteFunc: func(obj interface{}) {
			// Get the DNSNameResolver object.
			resolverObj, ok := obj.(*ocpnetworkapiv1alpha1.DNSNameResolver)
			if !ok {
				log.Infof("object not of type DNSNameResolver: %v", obj)
				return
			}

			// Check if namespace is configured or not.
			if !resolver.configuredNamespace(resolverObj.Namespace) {
				return
			}

			dnsName := string(resolverObj.Spec.Name)
			// Check if the DNS name is wildcard or regular.
			if isWildcard(dnsName) {
				// If the DNS name is wildcard, delete the details of the DNSNameResolver
				// object from the wildcardDNSInfo map.
				resolver.wildcardMapLock.Lock()
				if dnsInfoMap, exists := resolver.wildcardDNSInfo[dnsName]; exists {
					// If details of DNS name and the DNSNameResolver objects already exist
					// then check if the existing information match with the current one.
					// Otherwise, don't proceed.
					if dnsInfoMap[resolverObj.Namespace] == resolverObj.Name {
						delete(dnsInfoMap, resolverObj.Namespace)
						if len(dnsInfoMap) > 0 {
							resolver.wildcardDNSInfo[dnsName] = dnsInfoMap
						} else {
							delete(resolver.wildcardDNSInfo, dnsName)
						}
					}
				}
				resolver.wildcardMapLock.Unlock()
			} else {
				// If the DNS name is regular, delete the details of the DNSNameResolver
				// object from the regularDNSInfo map.
				resolver.regularMapLock.Lock()
				if dnsInfoMap, exists := resolver.regularDNSInfo[dnsName]; exists {
					// If details of DNS name and the DNSNameResolver objects already exist
					// then check if the existing information match with the current one.
					// Otherwise, don't proceed.
					if dnsInfoMap[resolverObj.Namespace] == resolverObj.Name {
						delete(dnsInfoMap, resolverObj.Namespace)
						if len(dnsInfoMap) > 0 {
							resolver.regularDNSInfo[dnsName] = dnsInfoMap
						} else {
							delete(resolver.regularDNSInfo, dnsName)
						}
					}
				}
				resolver.regularMapLock.Unlock()
			}

			// Used only in unit tests.
			if send != nil {
				send(resolverObj)
			}
		},
		// Used only in unit tests.
		// Update event.
		UpdateFunc: func(oldObj, newObj interface{}) {
			// Get the DNSNameResolver object.
			newResolverObj, ok := oldObj.(*ocpnetworkapiv1alpha1.DNSNameResolver)
			if !ok {
				log.Infof("object not of type DNSNameResolver: %v", oldObj)
				return
			}

			// Check if namespace is configured or not.
			if !resolver.configuredNamespace(newResolverObj.Namespace) {
				return
			}

			// Used only in unit tests.
			if send != nil {
				send(newResolverObj)
			}
		},
	})
	return nil
}

// initPlugin initializes the ocp_dnsnameresolver plugin and returns the plugin startup and
// shutdown callback functions.
func (resolver *OCPDNSNameResolver) initPlugin() (func() error, func() error, error) {
	// Create a client supporting network.openshift.io apis.
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, err
	}

	networkClient, err := ocpnetworkclient.NewForConfig(kubeConfig)
	if err != nil {
		return nil, nil, err
	}

	err = resolver.initInformer(networkClient, nil)
	if err != nil {
		return nil, nil, err
	}

	resolver.stopCh = make(chan struct{})

	onStart := func() error {
		go func() {
			resolver.dnsNameResolverInformer.Run(resolver.stopCh)
		}()

		timeout := 5 * time.Second
		timeoutTicker := time.NewTicker(timeout)
		defer timeoutTicker.Stop()
		logDelay := 500 * time.Millisecond
		logTicker := time.NewTicker(logDelay)
		defer logTicker.Stop()
		checkSyncTicker := time.NewTicker(100 * time.Millisecond)
		defer checkSyncTicker.Stop()
		for {
			select {
			case <-checkSyncTicker.C:
				if resolver.dnsNameResolverInformer.HasSynced() {
					return nil
				}
			case <-logTicker.C:
				log.Info("waiting for DNS Name Resolver Informer sync before starting server")
			case <-timeoutTicker.C:
				log.Warning("starting server with unsynced DNS Name Resolver Informer")
				return nil
			}
		}
	}

	onShut := func() error {
		resolver.stopLock.Lock()
		defer resolver.stopLock.Unlock()

		// Only try draining the workqueue if we haven't already.
		if !resolver.shutdown {
			close(resolver.stopCh)
			resolver.shutdown = true

			return nil
		}

		return fmt.Errorf("shutdown already in progress")
	}

	return onStart, onShut, nil
}
