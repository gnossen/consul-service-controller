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
var consulMaxAttempts = flag.Int("max-consul-attempts", 5, "The maximum number of times to attempt to register/deregister services with Consul.")
var consulWaitSeconds = flag.Int("attempt-wait-seconds", 2, "The number of seconds to wait between attempts.")

var httpClient = &http.Client{}

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
	log.Printf("Registering %s at %s.\n", hostname, ip)
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
	return nil
}

func retry(maxAttempts int, waitPeriod time.Duration, msg string, fn func() error) {
    for i := 0; i < maxAttempts; i++ {
        err := fn()
        if err == nil {
            // Don't clutter the logs if we succeed on the first try.
            if i != 0 {
                log.Printf("Success: %s\n", msg)
            }
            break
        }
        log.Printf("Failure: %s\nError: %s\n", msg, err)
        if i == maxAttempts -1 {
            log.Printf("Maximum of %d attempts reached. Giving up.\n", maxAttempts)
            break
        }
        log.Printf("Trying againt after %s.\n", waitPeriod)
        time.Sleep(waitPeriod)
    }
}

func registerHostname(hostname string, ip string) {
    retry(*consulMaxAttempts,
        time.Duration(*consulWaitSeconds) * time.Second,
        fmt.Sprintf("Register %s at %s", hostname, ip),
        func() error {
            return tryRegisterHostname(hostname, ip)
        })
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
	log.Printf("Deregistering %s.\n", hostname)
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
	return nil
}

func deregisterHostname(hostname string) {
    retry(*consulMaxAttempts,
        time.Duration(*consulWaitSeconds) * time.Second,
        fmt.Sprintf("Deregister %s", hostname),
        func() error {
            return tryDeregisterHostname(hostname)
        })
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
        time.Second * time.Duration(*pollSeconds),
        cache.ResourceEventHandlerFuncs{
            AddFunc: func(obj interface{}) {
                service := *obj.(*corev1.Service)
                if isServiceApplicable(service) {
                    registerService(service)
                    activeServices[getKey(service)] = getServiceAddress(service)
                }
            },
            DeleteFunc: func(obj interface{}) {
                service := *obj.(*corev1.Service)
                key := getKey(service)
                if _, ok := activeServices[key]; ok {
                    deregisterService(service)
                    delete(activeServices, key)
                }
            },
            UpdateFunc: func(oldObj, newObj interface{}) {
                oldService := *oldObj.(*corev1.Service)
                newService := *newObj.(*corev1.Service)
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

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

    updateOnDeltas(clientset)
}
