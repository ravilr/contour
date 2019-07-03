// Copyright © 2019 Heptio
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
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	accesslogv2 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v2"
	accesslog "github.com/envoyproxy/go-control-plane/envoy/config/filter/accesslog/v2"
	"github.com/envoyproxy/go-control-plane/pkg/util"
)

// FileAccessLog returns a new file based access log filter.
func FileAccessLog(path string) []*accesslog.AccessLog {
	return []*accesslog.AccessLog{{
		Name: util.FileAccessLog,
		Filter: accesslogFilter(),
		ConfigType: &accesslog.AccessLog_TypedConfig{
			TypedConfig: any(&accesslogv2.FileAccessLog{
				Path: path,
				// TODO(dfc) FileAccessLog_Format elided.
			}),
		},
	}}
}

func accesslogFilter() *accesslog.AccessLogFilter {
	return &accesslog.AccessLogFilter{
		FilterSpecifier: &accesslog.AccessLogFilter_OrFilter{
			OrFilter: &accesslog.OrFilter{
				Filters: []*accesslog.AccessLogFilter{{
					FilterSpecifier: &accesslog.AccessLogFilter_StatusCodeFilter{
						StatusCodeFilter: &accesslog.StatusCodeFilter{
							Comparison: &accesslog.ComparisonFilter{
								Op: accesslog.ComparisonFilter_GE,
								Value: &core.RuntimeUInt32{
									DefaultValue: 400,
									RuntimeKey:   "access_log.access_error.status",
								},
							},
						},
					},
				}, {
				    FilterSpecifier: &accesslog.AccessLogFilter_StatusCodeFilter{
						StatusCodeFilter: &accesslog.StatusCodeFilter{
							Comparison: &accesslog.ComparisonFilter{
								Op: accesslog.ComparisonFilter_EQ,
								Value: &core.RuntimeUInt32{
									DefaultValue: 0,
									RuntimeKey:   "access_log.access_error.status",
								},
							},
						},
					},
				}},
			},
		},
	}
}
