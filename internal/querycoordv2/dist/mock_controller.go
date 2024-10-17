// Code generated by mockery v2.46.0. DO NOT EDIT.

package dist

import (
	context "context"

	mock "github.com/stretchr/testify/mock"
)

// MockController is an autogenerated mock type for the Controller type
type MockController struct {
	mock.Mock
}

type MockController_Expecter struct {
	mock *mock.Mock
}

func (_m *MockController) EXPECT() *MockController_Expecter {
	return &MockController_Expecter{mock: &_m.Mock}
}

// Remove provides a mock function with given fields: nodeID
func (_m *MockController) Remove(nodeID int64) {
	_m.Called(nodeID)
}

// MockController_Remove_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'Remove'
type MockController_Remove_Call struct {
	*mock.Call
}

// Remove is a helper method to define mock.On call
//   - nodeID int64
func (_e *MockController_Expecter) Remove(nodeID interface{}) *MockController_Remove_Call {
	return &MockController_Remove_Call{Call: _e.mock.On("Remove", nodeID)}
}

func (_c *MockController_Remove_Call) Run(run func(nodeID int64)) *MockController_Remove_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(int64))
	})
	return _c
}

func (_c *MockController_Remove_Call) Return() *MockController_Remove_Call {
	_c.Call.Return()
	return _c
}

func (_c *MockController_Remove_Call) RunAndReturn(run func(int64)) *MockController_Remove_Call {
	_c.Call.Return(run)
	return _c
}

// StartDistInstance provides a mock function with given fields: ctx, nodeID
func (_m *MockController) StartDistInstance(ctx context.Context, nodeID int64) {
	_m.Called(ctx, nodeID)
}

// MockController_StartDistInstance_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'StartDistInstance'
type MockController_StartDistInstance_Call struct {
	*mock.Call
}

// StartDistInstance is a helper method to define mock.On call
//   - ctx context.Context
//   - nodeID int64
func (_e *MockController_Expecter) StartDistInstance(ctx interface{}, nodeID interface{}) *MockController_StartDistInstance_Call {
	return &MockController_StartDistInstance_Call{Call: _e.mock.On("StartDistInstance", ctx, nodeID)}
}

func (_c *MockController_StartDistInstance_Call) Run(run func(ctx context.Context, nodeID int64)) *MockController_StartDistInstance_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(context.Context), args[1].(int64))
	})
	return _c
}

func (_c *MockController_StartDistInstance_Call) Return() *MockController_StartDistInstance_Call {
	_c.Call.Return()
	return _c
}

func (_c *MockController_StartDistInstance_Call) RunAndReturn(run func(context.Context, int64)) *MockController_StartDistInstance_Call {
	_c.Call.Return(run)
	return _c
}

// Stop provides a mock function with given fields:
func (_m *MockController) Stop() {
	_m.Called()
}

// MockController_Stop_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'Stop'
type MockController_Stop_Call struct {
	*mock.Call
}

// Stop is a helper method to define mock.On call
func (_e *MockController_Expecter) Stop() *MockController_Stop_Call {
	return &MockController_Stop_Call{Call: _e.mock.On("Stop")}
}

func (_c *MockController_Stop_Call) Run(run func()) *MockController_Stop_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockController_Stop_Call) Return() *MockController_Stop_Call {
	_c.Call.Return()
	return _c
}

func (_c *MockController_Stop_Call) RunAndReturn(run func()) *MockController_Stop_Call {
	_c.Call.Return(run)
	return _c
}

// SyncAll provides a mock function with given fields: ctx
func (_m *MockController) SyncAll(ctx context.Context) {
	_m.Called(ctx)
}

// MockController_SyncAll_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'SyncAll'
type MockController_SyncAll_Call struct {
	*mock.Call
}

// SyncAll is a helper method to define mock.On call
//   - ctx context.Context
func (_e *MockController_Expecter) SyncAll(ctx interface{}) *MockController_SyncAll_Call {
	return &MockController_SyncAll_Call{Call: _e.mock.On("SyncAll", ctx)}
}

func (_c *MockController_SyncAll_Call) Run(run func(ctx context.Context)) *MockController_SyncAll_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(context.Context))
	})
	return _c
}

func (_c *MockController_SyncAll_Call) Return() *MockController_SyncAll_Call {
	_c.Call.Return()
	return _c
}

func (_c *MockController_SyncAll_Call) RunAndReturn(run func(context.Context)) *MockController_SyncAll_Call {
	_c.Call.Return(run)
	return _c
}

// NewMockController creates a new instance of MockController. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewMockController(t interface {
	mock.TestingT
	Cleanup(func())
}) *MockController {
	mock := &MockController{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
