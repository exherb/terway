/*
Copyright 2022 Terway Authors.

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

package vswitch

import (
	"context"
	"testing"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/vpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/AliyunContainerService/terway/pkg/aliyun/client/mocks"
)

func TestSwitchPool_GetOne(t *testing.T) {
	openAPI := mocks.NewVPC(t)
	openAPI.On("DescribeVSwitchByID", mock.Anything, "vsw-1").Return(&vpc.VSwitch{
		VSwitchId:               "vsw-1",
		ZoneId:                  "zone-1",
		AvailableIpAddressCount: 10,
	}, nil).Maybe()
	openAPI.On("DescribeVSwitchByID", mock.Anything, "vsw-2").Return(&vpc.VSwitch{
		VSwitchId:               "vsw-2",
		ZoneId:                  "zone-2",
		AvailableIpAddressCount: 10,
	}, nil).Maybe()
	openAPI.On("DescribeVSwitchByID", mock.Anything, "vsw-3").Return(&vpc.VSwitch{
		VSwitchId:               "vsw-3",
		ZoneId:                  "zone-2",
		AvailableIpAddressCount: 10,
	}, nil).Maybe()

	switchPool, err := NewSwitchPool(100, "100m")
	assert.NoError(t, err)

	for i := 0; i < 10; i++ {
		sw, err := switchPool.GetOne(context.Background(), openAPI, "zone-2", []string{"vsw-2", "vsw-3"}, &SelectOptions{
			IgnoreZone:          false,
			VSwitchSelectPolicy: VSwitchSelectionPolicyOrdered,
		})
		assert.NoError(t, err)
		assert.Equal(t, "vsw-2", sw.ID)
	}

	ids := make(map[string]struct{})
	for i := 0; i < 10; i++ {
		sw, err := switchPool.GetOne(context.Background(), openAPI, "zone-2", []string{"vsw-2", "vsw-3"}, &SelectOptions{
			IgnoreZone:          false,
			VSwitchSelectPolicy: VSwitchSelectionPolicyRandom,
		})
		assert.NoError(t, err)
		ids[sw.ID] = struct{}{}
	}

	assert.Equal(t, 2, len(ids))
}
