package main

import (
	"flag"
	"fmt"
    "k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
    corev1 "k8s.io/api/core/v1"
	"log"
	"path/filepath"
	"time"

	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
)

var pollSeconds = flag.Int("poll-seconds", 60, "The number of seconds between state-of-the-world updates.")
var consulEndpoint = flag.String("consul-endpoint", "sidecar:8500", "The endpoint of the consul service to register with.")

var httpClient = &http.Client{}

// TODO: Implement an in-memory datastore and diff against the state of the world to enable deregistration on poll.

// Must check address count before calling this function.
func getServiceAddress(service corev1.Service) string {
    return service.Status.LoadBalancer.Ingress[0].IP
}

func getAddressCount(service corev1.Service) int {
    return len(service.Status.LoadBalancer.Ingress)
}

func isServiceApplicable(service corev1.Service) bool {
    // Consul can only support a single address.
    return getAddressCount(service) == 1
}

func tryRegisterHostname(hostname string, ip string) error {
	log.Printf("Attempting to register %s at %s.\n", hostname, ip)
	endpoint := fmt.Sprintf("http://%s/v1/agent/service/register", *consulEndpoint)
	body, err := json.Marshal(map[string]string{
		"Name":    hostname,
		"Address": ip,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("PUT", endpoint, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("Registration was unsuccessful with code %d: %s\n", resp.StatusCode, bodyBytes)
	}
	log.Printf("Successfully registered.")
	return nil
}

func registerHostname(hostname string, ip string) {
	// TODO: Implement some sort of retry.
	if err := tryRegisterHostname(hostname, ip); err != nil {
		log.Printf("Error registering %s at %s: %v.\n", hostname, ip, err)
	}
}

func registerService(service corev1.Service) {
    hostname := service.Name
    namespace := service.Namespace
    ip := getServiceAddress(service)
    registerHostname(fmt.Sprintf("%s-%s", hostname, namespace), ip)
    if namespace == "default" {
        registerHostname(hostname, ip)
    }
}

func tryDeregisterHostname(hostname string) error {
	log.Printf("Attempting to deregister %s.\n", hostname)
	endpoint := fmt.Sprintf("http://%s/v1/agent/service/deregister/%s", *consulEndpoint, hostname)
	req, err := http.NewRequest("PUT", endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("Deregistration was unsuccessful with code %d: %s\n", resp.StatusCode, bodyBytes)
	}
	log.Printf("Successfully deregistered.")
	return nil
}

func deregisterHostname(hostname string) {
	// TODO: Implement some sort of retry.
	if err := tryDeregisterHostname(hostname); err != nil {
		log.Printf("Error deregistering %s: %v.\n", hostname, err)
	}
}

func deregisterService(service corev1.Service) {
    hostname := service.Name
    namespace := service.Namespace
    deregisterHostname(fmt.Sprintf("%s-%s", hostname, namespace))
    if namespace == "default" {
        deregisterHostname(hostname)
    }
}

func updateOnDeltas(clientset *kubernetes.Clientset) {
    watchlist := cache.NewListWatchFromClient(
        clientset.CoreV1().RESTClient(),
        string(corev1.ResourceServices),
        corev1.NamespaceAll,
        fields.Everything())

    // Presence in this janky hash set signifies that we have registered the
    // corresponding service in Consul and have not deregistered it.
    // Keys are of the form <NAMESPACE>/<SERVICE_NAME> and the value is the service address.
    activeServices := map[string]string{}
    getKey := func(service corev1.Service) string {
        return fmt.Sprintf("%s/%s", service.Namespace, service.Name)
    }

    _, controller := cache.NewInformer(
        watchlist,
        &corev1.Service{},
        // TODO: This may allow us to get rid of the other goroutine. Figure out if that's true.
        time.Second * time.Duration(*pollSeconds),
        cache.ResourceEventHandlerFuncs{
            AddFunc: func(obj interface{}) {
                service := *obj.(*corev1.Service)
                log.Printf("Received add for %s\n", service.Name)
                if isServiceApplicable(service) {
                    registerService(service)
                    activeServices[getKey(service)] = getServiceAddress(service)
                }
            },
            DeleteFunc: func(obj interface{}) {
                service := *obj.(*corev1.Service)
                log.Printf("Received delete for %s\n", service.Name)
                key := getKey(service)
                if _, ok := activeServices[key]; ok {
                    deregisterService(service)
                    delete(activeServices, key)
                }
            },
            UpdateFunc: func(oldObj, newObj interface{}) {
                oldService := *oldObj.(*corev1.Service)
                newService := *newObj.(*corev1.Service)
                log.Printf("Received update for %s\n", oldService.Name)
                _, existing := activeServices[getKey(oldService)]
                applicable := isServiceApplicable(newService)
                if existing {
                    oldAddress := activeServices[getKey(oldService)]
                    newAddress := getServiceAddress(newService)
                    if oldAddress != newAddress {
                        // This just acts as an override.
                        registerService(newService)
                        activeServices[getKey(newService)] = newAddress
                    }
                } else if applicable {
                    // Newly applicable. Treat like an add operation.
                    registerService(newService)
                    activeServices[getKey(newService)] = getServiceAddress(newService)
                }
            },
        },
    )
    controller.Run(make(chan struct{}))
}

func main() {
	// TODO; Support in-cluster auth.
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// TODO: Implement delta update in a goroutine.

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

    updateOnDeltas(clientset)
}
