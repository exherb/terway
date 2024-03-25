// Code generated by mockery v2.42.1. DO NOT EDIT.

package mocks

import (
	context "context"

	vpc "github.com/aliyun/alibaba-cloud-sdk-go/services/vpc"
	mock "github.com/stretchr/testify/mock"
)

// VPC is an autogenerated mock type for the VPC type
type VPC struct {
	mock.Mock
}

// DescribeVSwitchByID provides a mock function with given fields: ctx, vSwitchID
func (_m *VPC) DescribeVSwitchByID(ctx context.Context, vSwitchID string) (*vpc.VSwitch, error) {
	ret := _m.Called(ctx, vSwitchID)

	if len(ret) == 0 {
		panic("no return value specified for DescribeVSwitchByID")
	}

	var r0 *vpc.VSwitch
	var r1 error
	if rf, ok := ret.Get(0).(func(context.Context, string) (*vpc.VSwitch, error)); ok {
		return rf(ctx, vSwitchID)
	}
	if rf, ok := ret.Get(0).(func(context.Context, string) *vpc.VSwitch); ok {
		r0 = rf(ctx, vSwitchID)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*vpc.VSwitch)
		}
	}

	if rf, ok := ret.Get(1).(func(context.Context, string) error); ok {
		r1 = rf(ctx, vSwitchID)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// NewVPC creates a new instance of VPC. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewVPC(t interface {
	mock.TestingT
	Cleanup(func())
}) *VPC {
	mock := &VPC{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
