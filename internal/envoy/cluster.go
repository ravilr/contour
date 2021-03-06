// Copyright © 2018 Heptio
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

package envoy

import (
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_cluster "github.com/envoyproxy/go-control-plane/envoy/api/v2/cluster"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type"
	"github.com/gogo/protobuf/types"
	"github.com/heptio/contour/internal/dag"
)

// CACertificateKey stores the key for the TLS validation secret cert
const CACertificateKey = "ca.crt"

// Cluster creates new v2.Cluster from dag.Cluster.
func Cluster(c *dag.Cluster) *v2.Cluster {
	switch upstream := c.Upstream.(type) {
	case *dag.HTTPService:
		cl := cluster(c, &upstream.TCPService)
		switch upstream.Protocol {
		case "tls":
			cl.TlsContext = UpstreamTLSContext(
				upstreamValidationCACert(c),
				upstreamValidationSubjectAltName(c),
			)
		case "h2":
			cl.TlsContext = UpstreamTLSContext(
				upstreamValidationCACert(c),
				upstreamValidationSubjectAltName(c),
				"h2")
			fallthrough
		case "h2c":
			cl.Http2ProtocolOptions = &core.Http2ProtocolOptions{}
		}
		return cl
	case *dag.TCPService:
		return cluster(c, upstream)
	default:
		panic(fmt.Sprintf("unsupported upstream type: %T", upstream))
	}
}

func upstreamValidationCACert(c *dag.Cluster) []byte {
	if c.UpstreamValidation == nil {
		// No validation required
		return nil
	}
	return c.UpstreamValidation.CACertificate.Object.Data[CACertificateKey]
}

func upstreamValidationSubjectAltName(c *dag.Cluster) string {
	if c.UpstreamValidation == nil {
		// No validation required
		return ""
	}
	return c.UpstreamValidation.SubjectName
}

func cluster(cluster *dag.Cluster, service *dag.TCPService) *v2.Cluster {
	c := &v2.Cluster{
		Name:           Clustername(cluster),
		AltStatName:    altStatName(service),
		ConnectTimeout: 250 * time.Millisecond,
		LbPolicy:       lbPolicy(cluster.LoadBalancerStrategy),
		CommonLbConfig: ClusterCommonLBConfig(),
		HealthChecks:   edshealthcheck(cluster),
	}

	switch len(service.ExternalName) {
	case 0:
		// external name not set, cluster will be discovered via EDS
		c.ClusterDiscoveryType = ClusterDiscoveryType(v2.Cluster_EDS)
		c.EdsClusterConfig = edsconfig("contour", service)
	default:
		// external name set, use hard coded DNS name
		c.ClusterDiscoveryType = ClusterDiscoveryType(v2.Cluster_STRICT_DNS)
		c.LoadAssignment = StaticClusterLoadAssignment(service)
	}

	// Drain connections immediately if using healthchecks and the endpoint is known to be removed
	if cluster.HealthCheck != nil {
		c.DrainConnectionsOnHostRemoval = true
	}

	if anyPositive(service.MaxConnections, service.MaxPendingRequests, service.MaxRequests, service.MaxRetries) {
		c.CircuitBreakers = &envoy_cluster.CircuitBreakers{
			Thresholds: []*envoy_cluster.CircuitBreakers_Thresholds{{
				MaxConnections:     u32nil(service.MaxConnections),
				MaxPendingRequests: u32nil(service.MaxPendingRequests),
				MaxRequests:        u32nil(service.MaxRequests),
				MaxRetries:         u32nil(service.MaxRetries),
			}},
		}
	}
	return c
}

// StaticClusterLoadAssignment creates a *v2.ClusterLoadAssignment pointing to the external DNS address of the service
func StaticClusterLoadAssignment(service *dag.TCPService) *v2.ClusterLoadAssignment {
	name := []string{
		service.Namespace,
		service.Name,
		service.ServicePort.Name,
	}

	return &v2.ClusterLoadAssignment{
		ClusterName: strings.Join(name, "/"),
		Endpoints: []endpoint.LocalityLbEndpoints{{
			LbEndpoints: []endpoint.LbEndpoint{{
				HostIdentifier: &endpoint.LbEndpoint_Endpoint{
					Endpoint: &endpoint.Endpoint{
						Address: &core.Address{
							Address: &core.Address_SocketAddress{
								SocketAddress: &core.SocketAddress{
									Address: service.ExternalName,
									PortSpecifier: &core.SocketAddress_PortValue{
										PortValue: uint32(service.ServicePort.Port),
									},
								},
							},
						},
					},
				},
			}},
		}},
	}
}

func edsconfig(cluster string, service *dag.TCPService) *v2.Cluster_EdsClusterConfig {
	name := []string{
		service.Namespace,
		service.Name,
		service.ServicePort.Name,
	}
	if name[2] == "" {
		name = name[:2]
	}
	return &v2.Cluster_EdsClusterConfig{
		EdsConfig:   ConfigSource(cluster),
		ServiceName: strings.Join(name, "/"),
	}
}

func lbPolicy(strategy string) v2.Cluster_LbPolicy {
	switch strategy {
	case "WeightedLeastRequest":
		return v2.Cluster_LEAST_REQUEST
	case "Random":
		return v2.Cluster_RANDOM
	case "Cookie":
		return v2.Cluster_RING_HASH
	default:
		return v2.Cluster_ROUND_ROBIN
	}
}

func edshealthcheck(c *dag.Cluster) []*core.HealthCheck {
	if c.HealthCheck == nil {
		return nil
	}
	return []*core.HealthCheck{
		healthCheck(c),
	}
}

// Clustername returns the name of the CDS cluster for this service.
func Clustername(cluster *dag.Cluster) string {
	var service *dag.TCPService
	switch s := cluster.Upstream.(type) {
	case *dag.HTTPService:
		service = &s.TCPService
	case *dag.TCPService:
		service = s
	default:
		panic(fmt.Sprintf("unsupported upstream type: %T", s))
	}
	buf := cluster.LoadBalancerStrategy
	if hc := cluster.HealthCheck; hc != nil {
		if hc.TimeoutSeconds > 0 {
			buf += (time.Duration(hc.TimeoutSeconds) * time.Second).String()
		}
		if hc.IntervalSeconds > 0 {
			buf += (time.Duration(hc.IntervalSeconds) * time.Second).String()
		}
		if hc.UnhealthyThresholdCount > 0 {
			buf += strconv.Itoa(int(hc.UnhealthyThresholdCount))
		}
		if hc.HealthyThresholdCount > 0 {
			buf += strconv.Itoa(int(hc.HealthyThresholdCount))
		}
		buf += hc.Path
	}
	if uv := cluster.UpstreamValidation; uv != nil {
		buf += uv.CACertificate.Object.ObjectMeta.Name
		buf += uv.SubjectName
	}

	hash := sha1.Sum([]byte(buf))
	ns := service.Namespace
	name := service.Name
	return hashname(60, ns, name, strconv.Itoa(int(service.Port)), fmt.Sprintf("%x", hash[:5]))
}

// altStatName generates an alternative stat name for the service
// using format ns_name_port
func altStatName(service *dag.TCPService) string {
	return strings.Join([]string{service.Namespace, service.Name, strconv.Itoa(int(service.Port))}, "_")
}

// hashname takes a lenth l and a varargs of strings s and returns a string whose length
// which does not exceed l. Internally s is joined with strings.Join(s, "/"). If the
// combined length exceeds l then hashname truncates each element in s, starting from the
// end using a hash derived from the contents of s (not the current element). This process
// continues until the length of s does not exceed l, or all elements have been truncated.
// In which case, the entire string is replaced with a hash not exceeding the length of l.
func hashname(l int, s ...string) string {
	const shorthash = 6 // the length of the shorthash

	r := strings.Join(s, "/")
	if l > len(r) {
		// we're under the limit, nothing to do
		return r
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(r)))
	for n := len(s) - 1; n >= 0; n-- {
		s[n] = truncate(l/len(s), s[n], hash[:shorthash])
		r = strings.Join(s, "/")
		if l > len(r) {
			return r
		}
	}
	// truncated everything, but we're still too long
	// just return the hash truncated to l.
	return hash[:min(len(hash), l)]
}

// truncate truncates s to l length by replacing the
// end of s with -suffix.
func truncate(l int, s, suffix string) string {
	if l >= len(s) {
		// under the limit, nothing to do
		return s
	}
	if l <= len(suffix) {
		// easy case, just return the start of the suffix
		return suffix[:min(l, len(suffix))]
	}
	return s[:l-len(suffix)-1] + "-" + suffix
}

func min(a, b int) int {
	if a > b {
		return b
	}
	return a
}

// anyPositive indicates if any of the values provided are greater than zero.
func anyPositive(first int, rest ...int) bool {
	if first > 0 {
		return true
	}
	for _, v := range rest {
		if v > 0 {
			return true
		}
	}
	return false
}

// u32nil creates a *types.UInt32Value containing v.
// u32nil returns nil if v is zero.
func u32nil(val int) *types.UInt32Value {
	switch val {
	case 0:
		return nil
	default:
		return u32(val)
	}
}

// ClusterCommonLBConfig creates a *v2.Cluster_CommonLbConfig with HealthyPanicThreshold disabled.
func ClusterCommonLBConfig() *v2.Cluster_CommonLbConfig {
	return &v2.Cluster_CommonLbConfig{
		HealthyPanicThreshold: &envoy_type.Percent{ // Disable HealthyPanicThreshold
			Value: 0,
		},
	}
}

// ConfigSource returns a *core.ConfigSource for cluster.
func ConfigSource(cluster string) *core.ConfigSource {
	return &core.ConfigSource{
		ConfigSourceSpecifier: &core.ConfigSource_ApiConfigSource{
			ApiConfigSource: &core.ApiConfigSource{
				ApiType: core.ApiConfigSource_GRPC,
				GrpcServices: []*core.GrpcService{{
					TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
						EnvoyGrpc: &core.GrpcService_EnvoyGrpc{
							ClusterName: cluster,
						},
					},
				}},
			},
		},
	}
}

// ClusterDiscoveryType returns the type of a ClusterDiscovery as a Cluster_type.
func ClusterDiscoveryType(t v2.Cluster_DiscoveryType) *v2.Cluster_Type {
	return &v2.Cluster_Type{Type: t}
}
