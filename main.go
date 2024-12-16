package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"
	"github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/autoscaling"
    "github.com/aws/aws-sdk-go-v2/service/ec2"
)

type Server interface{
	Address() string
	IsAlive() bool
	Serve(rw http.ResponseWriter, r *http.Request)
}

type simpleServer struct {
	address string
	proxy *httputil.ReverseProxy

}

func newSimpleServer(address string) *simpleServer {
	serverUrl, err := url.Parse(address)
	handleErr(err)

	return &simpleServer {
		address: address,
		proxy: httputil.NewSingleHostReverseProxy(serverUrl),
	}
}

type LoadBalancer struct {
	port	string
	roundRobinCount 	int
	servers 	[]Server
	mutex           sync.RWMutex
}


func newLoadBalancer(port string, servers []Server) *LoadBalancer {
	return &LoadBalancer {
		port: port,
		roundRobinCount: 0,
		servers: servers,
	}
}

func handleErr(err error){
	if err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}

func (s *simpleServer) Address() string {return s.address}

func (s *simpleServer) IsAlive() bool {return true}

func (s *simpleServer) Serve(rw http.ResponseWriter, req *http.Request){
	s.proxy.ServeHTTP(rw, req)
}

func (lb *LoadBalancer) getNextAvailableSever() Server {
	server := lb.servers[lb.roundRobinCount % len(lb.servers)]
	for !server.IsAlive(){
		lb.roundRobinCount++
		server = lb.servers[lb.roundRobinCount % len(lb.servers)]
	}
	lb.roundRobinCount++
	return server
}

func (lb *LoadBalancer) serveProxy(rw http.ResponseWriter, req *http.Request){
	targetServer := lb.getNextAvailableSever()
	fmt.Printf("Forwarding request to %q\n", targetServer.Address())
	targetServer.Serve(rw, req)
}

func (lb *LoadBalancer) updateServers(newServers []Server) {
	lb.mutex.Lock()
	defer lb.mutex.Unlock()
	lb.servers = newServers
 }
 

func fetchEC2Instances(asgClient *autoscaling.Client, ec2Client *ec2.Client, groupName string) ([]Server, error) {
	output, err := asgClient.DescribeAutoScalingGroups(context.TODO(), &autoscaling.DescribeAutoScalingGroupsInput{ //gets list of instances managed by ASG
		AutoScalingGroupNames: []string{groupName},
	})
	if err != nil {
		return nil, err
	}
 
 
	asg := output.AutoScalingGroups[0]
 
 
	if len(asg.Instances) == 0 {
		log.Printf("ASG %s exists but has no managed instances", groupName)
		return []Server{}, nil
	}
 
 
	if len(output.AutoScalingGroups) == 0 {
		return nil, fmt.Errorf("no Auto Scaling Group found with name %s", groupName)
	}
 
 
	var instanceIDs []string
	for _, instance := range output.AutoScalingGroups[0].Instances {
		if instance.LifecycleState == "InService" {
			instanceIDs = append(instanceIDs, *instance.InstanceId)
		}
	}
 
 
	ec2Output, err := ec2Client.DescribeInstances(context.TODO(), &ec2.DescribeInstancesInput{ //retreives more information, like the IP address of each instance
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return nil, err
	}
 
 
	var servers []Server
	for _, reservation := range ec2Output.Reservations { //reservations represent a grouping of EC2 instances returned
		for _, instance := range reservation.Instances { //goes through each instance in reservation
			if instance.PublicIpAddress != nil {
				address := fmt.Sprintf("http://%s:8081", *instance.PublicIpAddress) // ec2 intance accepts requests to port 8081 for api
				servers = append(servers, newSimpleServer(address))
			}
		}
	}
 
 
	return servers, nil
 }
 

func main() {
	// AWS SDK Configuration
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion("us-east-2"))
	handleErr(err)
 
 
	asgClient := autoscaling.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
 
 
	// Auto Scaling Group Name
	autoScalingGroupName := "lbProjectASGTemplate"
 
 
	lb := newLoadBalancer("8000", []Server{})
 
 
	go func() {
		for {
			newServers, err := fetchEC2Instances(asgClient, ec2Client, autoScalingGroupName)
			if err != nil {
				log.Printf("Error updating servers: %v", err)
			} else {
				lb.updateServers(newServers)
				log.Printf("Updated server list: %v", newServers)
			}
			time.Sleep(1 * time.Minute) // Update every 5 minutes
		}
	}()
 
 
	handleRedirect := func(rw http.ResponseWriter, req *http.Request) {
		lb.serveProxy(rw, req)
	}
 
 
	http.HandleFunc("/", handleRedirect)
 
 
	fmt.Printf("Serving requests at 'localhost:%s'\n", lb.port)
	http.ListenAndServe(":"+lb.port, nil)
}