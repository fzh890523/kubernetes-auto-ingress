package main

import (
	"flag"
	"fmt"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	log "github.com/sirupsen/logrus"
)

var (
	//dns wildcard record for all applications created, should be like example.com
	wildcardRecord = os.Getenv("AUTO_INGRESS_SERVER_NAME")
	//secret for ssl/tls of namespace where auto-ingress is running
	secret = os.Getenv("AUTO_INGRESS_SECRET")
	//read kubeconfig
	kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
)

func main() {
	flag.Parse()

	var err error
	var config *rest.Config

	//if kubeconfig is specified, use out-of-cluster
	if *kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		//get config when running inside Kubernetes
		config, err = rest.InClusterConfig()
	}

	if err != nil {
		log.Errorln(err.Error())
		return
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Errorln(err.Error())
		return
	}

	//map to keep track of which services have been already auto-ingressed
	var svcIngPair map[string]extensions.Ingress
	svcIngPair = make(map[string]extensions.Ingress)

	//get current ingresses on cluster
	log.Info("Initializing mapping between ingresses and services...")
	err = createIngressServiceMap(clientset, svcIngPair)
	if err != nil {
		log.Errorln(err.Error())
		return
	}

	log.Info("Initialized map: ", reflect.ValueOf(svcIngPair).MapKeys())

	//create a watch to listen for create/update/delete event on service
	//new created service will be auto-ingressed if it specifies label "autoingress: true"
	//deleted service will be remove the associated ingress if it specifies label "autoingress: true"
	watchlist := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "services", "",
		fields.Everything())
	_, controller := cache.NewInformer(
		watchlist,
		&corev1.Service{},
		time.Second*0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				svc := obj.(*corev1.Service)
				log.Info("Service added: ", svc.Name)
				lb := svc.Labels
				if _, found1 := svcIngPair[svc.Name]; !found1 {
					if val, found2 := lb["auto-ingress/enabled"]; found2 {
						if val == "enabled" {
							newIng, err := createIngressForService(clientset, *svc)
							if err != nil {
								log.Errorln(err.Error())
							} else {
								log.Info("Created new ingress for service: ", svc.Name)
								svcIngPair[svc.Name] = *newIng
								log.Info("Updated map: ", reflect.ValueOf(svcIngPair).MapKeys())
							}
						}
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				svc := obj.(*corev1.Service)
				log.Info("Service deleted: ", svc.Name)
				if ing, found := svcIngPair[svc.Name]; found {
					clientset.ExtensionsV1beta1().Ingresses(svc.Namespace).Delete(ing.Name, nil)
					log.Info("Deleted ingress for service: ", svc.Name)
					delete(svcIngPair, svc.Name)
					log.Info("Updated map: ", reflect.ValueOf(svcIngPair).MapKeys())
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				newSvc := newObj.(*corev1.Service)
				log.Info("Service changed: ", newSvc.Name)
				lb := newSvc.Labels
				if ing, found1 := svcIngPair[newSvc.Name]; found1 {
					if val, found2 := lb["auto-ingress/enabled"]; !found2 {
						clientset.ExtensionsV1beta1().Ingresses(newSvc.Namespace).Delete(ing.Name, nil)
						log.Info("Deleted ingress for service: ", newSvc.Name)
						delete(svcIngPair, newSvc.Name)
						log.Info("Updated map: ", reflect.ValueOf(svcIngPair).MapKeys())
					} else {
						if val == "disabled" {
							clientset.ExtensionsV1beta1().Ingresses(newSvc.Namespace).Delete(ing.Name, nil)
							log.Info("Deleted ingress for service: ", newSvc.Name)
							delete(svcIngPair, newSvc.Name)
							log.Info("Updated map: ", reflect.ValueOf(svcIngPair).MapKeys())
						}
					}
				} else {
					if val, found3 := lb["auto-ingress/enabled"]; found3 {
						if val == "enabled" {
							newIng, err := createIngressForService(clientset, *newSvc)
							if err != nil {
								log.Errorln(err.Error())
							} else {
								log.Info("created new ingress for service: ", newSvc.Name)
								svcIngPair[newSvc.Name] = *newIng
								log.Info("Updated map: ", reflect.ValueOf(svcIngPair).MapKeys())
							}
						}
					}
				}
			},
		},
	)
	stop := make(chan struct{})
	go controller.Run(stop)
	for {
		time.Sleep(time.Second)
	}
}

//create service map in the initial phase to check the current ingresses running on cluster
func createIngressServiceMap(clientset *kubernetes.Clientset, m map[string]extensions.Ingress) error {

	services, err := clientset.CoreV1().Services("").List(metav1.ListOptions{})

	if err != nil {
		return err
	}

	//get ingresses from all namespaces
	ingresses, err := clientset.ExtensionsV1beta1().Ingresses("").List(metav1.ListOptions{})

	if err != nil {
		return err
	}

	//get all services which have "auto-ingress/enabled" labels and their associated ingresses
	for i := 0; i < len(ingresses.Items); i++ {
		rules := ingresses.Items[i].Spec.Rules
		for j := 0; j < len(rules); j++ {
			paths := rules[j].HTTP.Paths
			for k := 0; k < len(paths); k++ {
				svcName := paths[k].Backend.ServiceName
				if _, found := m[svcName]; !found {
					m[svcName] = ingresses.Items[i]
				}
			}
		}
	}

	//if there is any services with the label "auto-ingress/enabled" but haven't had the ingresses, create them.
	for i := 0; i < len(services.Items); i++ {
		lb := services.Items[i].GetLabels()
		svcName := services.Items[i].GetName()
		if _, found1 := m[svcName]; !found1 {
			if val, found2 := lb["auto-ingress/enabled"]; found2 {
				if val == "enabled" {
					newIng, err := createIngressForService(clientset, services.Items[i])
					if err != nil {
						return err
					}
					m[services.Items[i].GetName()] = *newIng
				}
			}
		} else {
			if val, found2 := lb["auto-ingress/enabled"]; found2 {
				if val == "disabled" {
					delete(m, svcName)
				}
			} else {
				delete(m, svcName)
			}
		}
	}

	return nil
}

//create an ingress for the associated service
func createIngressForService(clientset *kubernetes.Clientset, service corev1.Service) (*extensions.Ingress, error) {
	lbl := service.Labels
	var (
		httpPorts, httpsPorts []int
		https                 bool
		err                   error
	)
	if lbl != nil {
		if v := lbl["auto-ingress/httpPorts"]; v != "" {
			httpPorts, err = parsePortsStr(v)
			if err != nil {
				return nil, fmt.Errorf("invalid httpPorts: %s", v)
			}
		}
		if v := lbl["auto-ingress/httpsPorts"]; v != "" {
			httpsPorts, err = parsePortsStr(v)
			if err != nil {
				return nil, fmt.Errorf("invalid httpsPorts: %s", v)
			}
		}
		if v := lbl["auto-ingress/httpsPorts"]; v != "" {
			if https, err = strconv.ParseBool(v); err != nil {
				return nil, fmt.Errorf("invalid https: %s", v)
			}
		}
	}
	backend := createIngressBackend(service, httpPorts, httpsPorts)

	ingress := createIngress(service, backend, https)

	newIng, err := clientset.ExtensionsV1beta1().Ingresses(service.Namespace).Create(ingress)

	return newIng, err
}

func parsePortsStr(s string) ([]int, error) {
	var ret []int
	for _, p := range strings.Split(s, ",") {
		p = strings.Trim(p, " ")
		if p == "" {
			continue
		}
		port, err := strconv.Atoi(p)
		if err != nil {
			return ret, err
		}
		ret = append(ret, port)
	}
	return ret, nil
}

//create an ingress backend before putting it to the ingress
func createIngressBackend(service corev1.Service, httpPorts, httpsPorts []int) extensions.IngressBackend {
	serviceName := service.GetName()
	var ret extensions.IngressBackend

	for _, p := range service.Spec.Ports {
		var match bool
		if httpPorts != nil || httpsPorts != nil {
			for _, hp := range httpPorts {
				if hp == int(p.Port) {
					match = true
					break
				}
			}
			if !match {
				for _, hp := range httpsPorts {
					if hp == int(p.Port) {
						match = true
						break
					}
				}
			}
		} else {
			match = true
		}

		if match {
			return extensions.IngressBackend{
				ServiceName: serviceName,
				ServicePort: intstr.FromInt(int(p.Port)),
			}
		}
	}

	return ret
}

//create ingress for associated service
func createIngress(service corev1.Service, backend extensions.IngressBackend, https bool) *extensions.Ingress {

	ingressname := service.Name
	servername := ingressname + "." + service.Namespace + "." + wildcardRecord

	var tls []extensions.IngressTLS
	if https {
		tls = append(tls, extensions.IngressTLS{
			Hosts: []string{
				servername,
			},
			SecretName: secret,
		})
	}

	return &extensions.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressname,
			Namespace: service.Namespace,
		},
		Spec: extensions.IngressSpec{
			TLS: tls,
			Rules: []extensions.IngressRule{
				{
					Host: servername,
					IngressRuleValue: extensions.IngressRuleValue{
						HTTP: &extensions.HTTPIngressRuleValue{
							Paths: []extensions.HTTPIngressPath{
								{
									Path:    "/",
									Backend: backend,
								},
							},
						},
					},
				},
			},
		},
	}
}
