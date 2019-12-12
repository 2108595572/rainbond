// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package healthy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	endpointapi "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	envoyv2 "github.com/goodrain/rainbond/node/core/envoy/v2"
	"google.golang.org/grpc"
)

//DependServiceHealthController Detect the health of the dependent service
//Health based conditions：
//------- lds: discover all dependent services
//------- cds: discover all dependent services
//------- sds: every service has at least one Ready instance
type DependServiceHealthController struct {
	listeners                 []v2.Listener
	clusters                  []v2.Cluster
	sdsHost                   []v2.ClusterLoadAssignment
	interval                  time.Duration
	envoyDiscoverVersion      string //only support v2
	checkFunc                 []func() bool
	endpointClient            v2.EndpointDiscoveryServiceClient
	dependServiceCount        int
	clusterID                 string
	dependServiceClusterNames []string
}

//NewDependServiceHealthController create a controller
func NewDependServiceHealthController() (*DependServiceHealthController, error) {
	clusterID := os.Getenv("ENVOY_NODE_ID")
	if clusterID == "" {
		clusterID = fmt.Sprintf("%s_%s_%s", os.Getenv("TENANT_ID"), os.Getenv("PLUGIN_ID"), os.Getenv("SERVICE_NAME"))
	}
	dsc := DependServiceHealthController{
		interval:  time.Second * 5,
		clusterID: clusterID,
	}
	dsc.checkFunc = append(dsc.checkFunc, dsc.checkListener)
	dsc.checkFunc = append(dsc.checkFunc, dsc.checkClusters)
	dsc.checkFunc = append(dsc.checkFunc, dsc.checkEDS)
	xDSHost := os.Getenv("XDS_HOST_IP")
	if xDSHost == "" {
		xDSHost = "172.30.42.1"
	}
	xDSHostPort := os.Getenv("XDS_HOST_PORT")
	if xDSHostPort == "" {
		xDSHostPort = "6101"
	}
	cli, err := grpc.Dial(fmt.Sprintf("%s:%s", xDSHost, xDSHostPort), grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	dsc.endpointClient = v2.NewEndpointDiscoveryServiceClient(cli)

	dsc.dependServiceClusterNames = strings.Split(os.Getenv("DEPEND_SERVICE_CLUSTER_NAMES"), ",")

	return &dsc, nil
}

//Check check all conditions
func (d *DependServiceHealthController) Check() {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	check := func() bool {
		for _, check := range d.checkFunc {
			if !check() {
				return false
			}
		}
		return true
	}
	for {
		if check() {
			logrus.Info("Depend services all check passed,will start service")
			return
		}
		select {
		case <-ticker.C:
		}
	}
}

func (d *DependServiceHealthController) checkListener() bool {
	return true
}

func (d *DependServiceHealthController) checkClusters() bool {
	return true
}

func (d *DependServiceHealthController) checkEDS() bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res, err := d.endpointClient.FetchEndpoints(ctx, &v2.DiscoveryRequest{
		Node: &core.Node{
			Cluster: d.clusterID,
			Id:      d.clusterID,
		},
	})
	if err != nil {
		logrus.Errorf("discover depend services endpoint failure %s", err.Error())
		return false
	}

	clusterLoadAssignments := envoyv2.ParseLocalityLbEndpointsResource(res.Resources)
	readyClusters := make(map[string]bool, len(clusterLoadAssignments))
	for _, cla := range clusterLoadAssignments {
		// clusterName := fmt.Sprintf("%s_%s_%s_%d", namespace, serviceAlias, destServiceAlias, service.Spec.Ports[0].Port)
		clusterName := cla.GetClusterName()
		ready := func() bool {
			if len(cla.Endpoints) > 0 && len(cla.Endpoints[0].LbEndpoints) > 0 {
				// first LbEndpoints healthy is not nil. so endpoint is not notreadyaddress
				if host, ok := cla.Endpoints[0].LbEndpoints[0].HostIdentifier.(*endpointapi.LbEndpoint_Endpoint); ok {
					if host.Endpoint != nil && host.Endpoint.HealthCheckConfig != nil {
						logrus.Infof("depend service (%s) start complete", cla.ClusterName)
						return true
					}
				}
			}
			return false
		}()
		readyClusters[clusterName] = ready
	}

	for _, cn := range d.dependServiceClusterNames {
		if _, ok := readyClusters[cn]; !ok {
			return false
		}
	}

	logrus.Info("all dependent services have been started.")

	return true
}
