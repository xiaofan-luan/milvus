// Code generated by mockery v2.46.0. DO NOT EDIT.

package mock_message

import (
	message "github.com/milvus-io/milvus/pkg/streaming/util/message"
	mock "github.com/stretchr/testify/mock"
)

// MockMutableMessage is an autogenerated mock type for the MutableMessage type
type MockMutableMessage struct {
	mock.Mock
}

type MockMutableMessage_Expecter struct {
	mock *mock.Mock
}

func (_m *MockMutableMessage) EXPECT() *MockMutableMessage_Expecter {
	return &MockMutableMessage_Expecter{mock: &_m.Mock}
}

// BarrierTimeTick provides a mock function with given fields:
func (_m *MockMutableMessage) BarrierTimeTick() uint64 {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for BarrierTimeTick")
	}

	var r0 uint64
	if rf, ok := ret.Get(0).(func() uint64); ok {
		r0 = rf()
	} else {
		r0 = ret.Get(0).(uint64)
	}

	return r0
}

// MockMutableMessage_BarrierTimeTick_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'BarrierTimeTick'
type MockMutableMessage_BarrierTimeTick_Call struct {
	*mock.Call
}

// BarrierTimeTick is a helper method to define mock.On call
func (_e *MockMutableMessage_Expecter) BarrierTimeTick() *MockMutableMessage_BarrierTimeTick_Call {
	return &MockMutableMessage_BarrierTimeTick_Call{Call: _e.mock.On("BarrierTimeTick")}
}

func (_c *MockMutableMessage_BarrierTimeTick_Call) Run(run func()) *MockMutableMessage_BarrierTimeTick_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockMutableMessage_BarrierTimeTick_Call) Return(_a0 uint64) *MockMutableMessage_BarrierTimeTick_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_BarrierTimeTick_Call) RunAndReturn(run func() uint64) *MockMutableMessage_BarrierTimeTick_Call {
	_c.Call.Return(run)
	return _c
}

// EstimateSize provides a mock function with given fields:
func (_m *MockMutableMessage) EstimateSize() int {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for EstimateSize")
	}

	var r0 int
	if rf, ok := ret.Get(0).(func() int); ok {
		r0 = rf()
	} else {
		r0 = ret.Get(0).(int)
	}

	return r0
}

// MockMutableMessage_EstimateSize_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'EstimateSize'
type MockMutableMessage_EstimateSize_Call struct {
	*mock.Call
}

// EstimateSize is a helper method to define mock.On call
func (_e *MockMutableMessage_Expecter) EstimateSize() *MockMutableMessage_EstimateSize_Call {
	return &MockMutableMessage_EstimateSize_Call{Call: _e.mock.On("EstimateSize")}
}

func (_c *MockMutableMessage_EstimateSize_Call) Run(run func()) *MockMutableMessage_EstimateSize_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockMutableMessage_EstimateSize_Call) Return(_a0 int) *MockMutableMessage_EstimateSize_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_EstimateSize_Call) RunAndReturn(run func() int) *MockMutableMessage_EstimateSize_Call {
	_c.Call.Return(run)
	return _c
}

// IntoImmutableMessage provides a mock function with given fields: msgID
func (_m *MockMutableMessage) IntoImmutableMessage(msgID message.MessageID) message.ImmutableMessage {
	ret := _m.Called(msgID)

	if len(ret) == 0 {
		panic("no return value specified for IntoImmutableMessage")
	}

	var r0 message.ImmutableMessage
	if rf, ok := ret.Get(0).(func(message.MessageID) message.ImmutableMessage); ok {
		r0 = rf(msgID)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(message.ImmutableMessage)
		}
	}

	return r0
}

// MockMutableMessage_IntoImmutableMessage_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'IntoImmutableMessage'
type MockMutableMessage_IntoImmutableMessage_Call struct {
	*mock.Call
}

// IntoImmutableMessage is a helper method to define mock.On call
//   - msgID message.MessageID
func (_e *MockMutableMessage_Expecter) IntoImmutableMessage(msgID interface{}) *MockMutableMessage_IntoImmutableMessage_Call {
	return &MockMutableMessage_IntoImmutableMessage_Call{Call: _e.mock.On("IntoImmutableMessage", msgID)}
}

func (_c *MockMutableMessage_IntoImmutableMessage_Call) Run(run func(msgID message.MessageID)) *MockMutableMessage_IntoImmutableMessage_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(message.MessageID))
	})
	return _c
}

func (_c *MockMutableMessage_IntoImmutableMessage_Call) Return(_a0 message.ImmutableMessage) *MockMutableMessage_IntoImmutableMessage_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_IntoImmutableMessage_Call) RunAndReturn(run func(message.MessageID) message.ImmutableMessage) *MockMutableMessage_IntoImmutableMessage_Call {
	_c.Call.Return(run)
	return _c
}

// MessageType provides a mock function with given fields:
func (_m *MockMutableMessage) MessageType() message.MessageType {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for MessageType")
	}

	var r0 message.MessageType
	if rf, ok := ret.Get(0).(func() message.MessageType); ok {
		r0 = rf()
	} else {
		r0 = ret.Get(0).(message.MessageType)
	}

	return r0
}

// MockMutableMessage_MessageType_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'MessageType'
type MockMutableMessage_MessageType_Call struct {
	*mock.Call
}

// MessageType is a helper method to define mock.On call
func (_e *MockMutableMessage_Expecter) MessageType() *MockMutableMessage_MessageType_Call {
	return &MockMutableMessage_MessageType_Call{Call: _e.mock.On("MessageType")}
}

func (_c *MockMutableMessage_MessageType_Call) Run(run func()) *MockMutableMessage_MessageType_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockMutableMessage_MessageType_Call) Return(_a0 message.MessageType) *MockMutableMessage_MessageType_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_MessageType_Call) RunAndReturn(run func() message.MessageType) *MockMutableMessage_MessageType_Call {
	_c.Call.Return(run)
	return _c
}

// Payload provides a mock function with given fields:
func (_m *MockMutableMessage) Payload() []byte {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for Payload")
	}

	var r0 []byte
	if rf, ok := ret.Get(0).(func() []byte); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).([]byte)
		}
	}

	return r0
}

// MockMutableMessage_Payload_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'Payload'
type MockMutableMessage_Payload_Call struct {
	*mock.Call
}

// Payload is a helper method to define mock.On call
func (_e *MockMutableMessage_Expecter) Payload() *MockMutableMessage_Payload_Call {
	return &MockMutableMessage_Payload_Call{Call: _e.mock.On("Payload")}
}

func (_c *MockMutableMessage_Payload_Call) Run(run func()) *MockMutableMessage_Payload_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockMutableMessage_Payload_Call) Return(_a0 []byte) *MockMutableMessage_Payload_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_Payload_Call) RunAndReturn(run func() []byte) *MockMutableMessage_Payload_Call {
	_c.Call.Return(run)
	return _c
}

// Properties provides a mock function with given fields:
func (_m *MockMutableMessage) Properties() message.RProperties {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for Properties")
	}

	var r0 message.RProperties
	if rf, ok := ret.Get(0).(func() message.RProperties); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(message.RProperties)
		}
	}

	return r0
}

// MockMutableMessage_Properties_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'Properties'
type MockMutableMessage_Properties_Call struct {
	*mock.Call
}

// Properties is a helper method to define mock.On call
func (_e *MockMutableMessage_Expecter) Properties() *MockMutableMessage_Properties_Call {
	return &MockMutableMessage_Properties_Call{Call: _e.mock.On("Properties")}
}

func (_c *MockMutableMessage_Properties_Call) Run(run func()) *MockMutableMessage_Properties_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockMutableMessage_Properties_Call) Return(_a0 message.RProperties) *MockMutableMessage_Properties_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_Properties_Call) RunAndReturn(run func() message.RProperties) *MockMutableMessage_Properties_Call {
	_c.Call.Return(run)
	return _c
}

// TimeTick provides a mock function with given fields:
func (_m *MockMutableMessage) TimeTick() uint64 {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for TimeTick")
	}

	var r0 uint64
	if rf, ok := ret.Get(0).(func() uint64); ok {
		r0 = rf()
	} else {
		r0 = ret.Get(0).(uint64)
	}

	return r0
}

// MockMutableMessage_TimeTick_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'TimeTick'
type MockMutableMessage_TimeTick_Call struct {
	*mock.Call
}

// TimeTick is a helper method to define mock.On call
func (_e *MockMutableMessage_Expecter) TimeTick() *MockMutableMessage_TimeTick_Call {
	return &MockMutableMessage_TimeTick_Call{Call: _e.mock.On("TimeTick")}
}

func (_c *MockMutableMessage_TimeTick_Call) Run(run func()) *MockMutableMessage_TimeTick_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockMutableMessage_TimeTick_Call) Return(_a0 uint64) *MockMutableMessage_TimeTick_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_TimeTick_Call) RunAndReturn(run func() uint64) *MockMutableMessage_TimeTick_Call {
	_c.Call.Return(run)
	return _c
}

// TxnContext provides a mock function with given fields:
func (_m *MockMutableMessage) TxnContext() *message.TxnContext {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for TxnContext")
	}

	var r0 *message.TxnContext
	if rf, ok := ret.Get(0).(func() *message.TxnContext); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*message.TxnContext)
		}
	}

	return r0
}

// MockMutableMessage_TxnContext_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'TxnContext'
type MockMutableMessage_TxnContext_Call struct {
	*mock.Call
}

// TxnContext is a helper method to define mock.On call
func (_e *MockMutableMessage_Expecter) TxnContext() *MockMutableMessage_TxnContext_Call {
	return &MockMutableMessage_TxnContext_Call{Call: _e.mock.On("TxnContext")}
}

func (_c *MockMutableMessage_TxnContext_Call) Run(run func()) *MockMutableMessage_TxnContext_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockMutableMessage_TxnContext_Call) Return(_a0 *message.TxnContext) *MockMutableMessage_TxnContext_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_TxnContext_Call) RunAndReturn(run func() *message.TxnContext) *MockMutableMessage_TxnContext_Call {
	_c.Call.Return(run)
	return _c
}

// VChannel provides a mock function with given fields:
func (_m *MockMutableMessage) VChannel() string {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for VChannel")
	}

	var r0 string
	if rf, ok := ret.Get(0).(func() string); ok {
		r0 = rf()
	} else {
		r0 = ret.Get(0).(string)
	}

	return r0
}

// MockMutableMessage_VChannel_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'VChannel'
type MockMutableMessage_VChannel_Call struct {
	*mock.Call
}

// VChannel is a helper method to define mock.On call
func (_e *MockMutableMessage_Expecter) VChannel() *MockMutableMessage_VChannel_Call {
	return &MockMutableMessage_VChannel_Call{Call: _e.mock.On("VChannel")}
}

func (_c *MockMutableMessage_VChannel_Call) Run(run func()) *MockMutableMessage_VChannel_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockMutableMessage_VChannel_Call) Return(_a0 string) *MockMutableMessage_VChannel_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_VChannel_Call) RunAndReturn(run func() string) *MockMutableMessage_VChannel_Call {
	_c.Call.Return(run)
	return _c
}

// Version provides a mock function with given fields:
func (_m *MockMutableMessage) Version() message.Version {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for Version")
	}

	var r0 message.Version
	if rf, ok := ret.Get(0).(func() message.Version); ok {
		r0 = rf()
	} else {
		r0 = ret.Get(0).(message.Version)
	}

	return r0
}

// MockMutableMessage_Version_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'Version'
type MockMutableMessage_Version_Call struct {
	*mock.Call
}

// Version is a helper method to define mock.On call
func (_e *MockMutableMessage_Expecter) Version() *MockMutableMessage_Version_Call {
	return &MockMutableMessage_Version_Call{Call: _e.mock.On("Version")}
}

func (_c *MockMutableMessage_Version_Call) Run(run func()) *MockMutableMessage_Version_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockMutableMessage_Version_Call) Return(_a0 message.Version) *MockMutableMessage_Version_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_Version_Call) RunAndReturn(run func() message.Version) *MockMutableMessage_Version_Call {
	_c.Call.Return(run)
	return _c
}

// WithBarrierTimeTick provides a mock function with given fields: tt
func (_m *MockMutableMessage) WithBarrierTimeTick(tt uint64) message.MutableMessage {
	ret := _m.Called(tt)

	if len(ret) == 0 {
		panic("no return value specified for WithBarrierTimeTick")
	}

	var r0 message.MutableMessage
	if rf, ok := ret.Get(0).(func(uint64) message.MutableMessage); ok {
		r0 = rf(tt)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(message.MutableMessage)
		}
	}

	return r0
}

// MockMutableMessage_WithBarrierTimeTick_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'WithBarrierTimeTick'
type MockMutableMessage_WithBarrierTimeTick_Call struct {
	*mock.Call
}

// WithBarrierTimeTick is a helper method to define mock.On call
//   - tt uint64
func (_e *MockMutableMessage_Expecter) WithBarrierTimeTick(tt interface{}) *MockMutableMessage_WithBarrierTimeTick_Call {
	return &MockMutableMessage_WithBarrierTimeTick_Call{Call: _e.mock.On("WithBarrierTimeTick", tt)}
}

func (_c *MockMutableMessage_WithBarrierTimeTick_Call) Run(run func(tt uint64)) *MockMutableMessage_WithBarrierTimeTick_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(uint64))
	})
	return _c
}

func (_c *MockMutableMessage_WithBarrierTimeTick_Call) Return(_a0 message.MutableMessage) *MockMutableMessage_WithBarrierTimeTick_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_WithBarrierTimeTick_Call) RunAndReturn(run func(uint64) message.MutableMessage) *MockMutableMessage_WithBarrierTimeTick_Call {
	_c.Call.Return(run)
	return _c
}

// WithLastConfirmed provides a mock function with given fields: id
func (_m *MockMutableMessage) WithLastConfirmed(id message.MessageID) message.MutableMessage {
	ret := _m.Called(id)

	if len(ret) == 0 {
		panic("no return value specified for WithLastConfirmed")
	}

	var r0 message.MutableMessage
	if rf, ok := ret.Get(0).(func(message.MessageID) message.MutableMessage); ok {
		r0 = rf(id)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(message.MutableMessage)
		}
	}

	return r0
}

// MockMutableMessage_WithLastConfirmed_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'WithLastConfirmed'
type MockMutableMessage_WithLastConfirmed_Call struct {
	*mock.Call
}

// WithLastConfirmed is a helper method to define mock.On call
//   - id message.MessageID
func (_e *MockMutableMessage_Expecter) WithLastConfirmed(id interface{}) *MockMutableMessage_WithLastConfirmed_Call {
	return &MockMutableMessage_WithLastConfirmed_Call{Call: _e.mock.On("WithLastConfirmed", id)}
}

func (_c *MockMutableMessage_WithLastConfirmed_Call) Run(run func(id message.MessageID)) *MockMutableMessage_WithLastConfirmed_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(message.MessageID))
	})
	return _c
}

func (_c *MockMutableMessage_WithLastConfirmed_Call) Return(_a0 message.MutableMessage) *MockMutableMessage_WithLastConfirmed_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_WithLastConfirmed_Call) RunAndReturn(run func(message.MessageID) message.MutableMessage) *MockMutableMessage_WithLastConfirmed_Call {
	_c.Call.Return(run)
	return _c
}

// WithLastConfirmedUseMessageID provides a mock function with given fields:
func (_m *MockMutableMessage) WithLastConfirmedUseMessageID() message.MutableMessage {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for WithLastConfirmedUseMessageID")
	}

	var r0 message.MutableMessage
	if rf, ok := ret.Get(0).(func() message.MutableMessage); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(message.MutableMessage)
		}
	}

	return r0
}

// MockMutableMessage_WithLastConfirmedUseMessageID_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'WithLastConfirmedUseMessageID'
type MockMutableMessage_WithLastConfirmedUseMessageID_Call struct {
	*mock.Call
}

// WithLastConfirmedUseMessageID is a helper method to define mock.On call
func (_e *MockMutableMessage_Expecter) WithLastConfirmedUseMessageID() *MockMutableMessage_WithLastConfirmedUseMessageID_Call {
	return &MockMutableMessage_WithLastConfirmedUseMessageID_Call{Call: _e.mock.On("WithLastConfirmedUseMessageID")}
}

func (_c *MockMutableMessage_WithLastConfirmedUseMessageID_Call) Run(run func()) *MockMutableMessage_WithLastConfirmedUseMessageID_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *MockMutableMessage_WithLastConfirmedUseMessageID_Call) Return(_a0 message.MutableMessage) *MockMutableMessage_WithLastConfirmedUseMessageID_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_WithLastConfirmedUseMessageID_Call) RunAndReturn(run func() message.MutableMessage) *MockMutableMessage_WithLastConfirmedUseMessageID_Call {
	_c.Call.Return(run)
	return _c
}

// WithTimeTick provides a mock function with given fields: tt
func (_m *MockMutableMessage) WithTimeTick(tt uint64) message.MutableMessage {
	ret := _m.Called(tt)

	if len(ret) == 0 {
		panic("no return value specified for WithTimeTick")
	}

	var r0 message.MutableMessage
	if rf, ok := ret.Get(0).(func(uint64) message.MutableMessage); ok {
		r0 = rf(tt)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(message.MutableMessage)
		}
	}

	return r0
}

// MockMutableMessage_WithTimeTick_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'WithTimeTick'
type MockMutableMessage_WithTimeTick_Call struct {
	*mock.Call
}

// WithTimeTick is a helper method to define mock.On call
//   - tt uint64
func (_e *MockMutableMessage_Expecter) WithTimeTick(tt interface{}) *MockMutableMessage_WithTimeTick_Call {
	return &MockMutableMessage_WithTimeTick_Call{Call: _e.mock.On("WithTimeTick", tt)}
}

func (_c *MockMutableMessage_WithTimeTick_Call) Run(run func(tt uint64)) *MockMutableMessage_WithTimeTick_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(uint64))
	})
	return _c
}

func (_c *MockMutableMessage_WithTimeTick_Call) Return(_a0 message.MutableMessage) *MockMutableMessage_WithTimeTick_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_WithTimeTick_Call) RunAndReturn(run func(uint64) message.MutableMessage) *MockMutableMessage_WithTimeTick_Call {
	_c.Call.Return(run)
	return _c
}

// WithTxnContext provides a mock function with given fields: txnCtx
func (_m *MockMutableMessage) WithTxnContext(txnCtx message.TxnContext) message.MutableMessage {
	ret := _m.Called(txnCtx)

	if len(ret) == 0 {
		panic("no return value specified for WithTxnContext")
	}

	var r0 message.MutableMessage
	if rf, ok := ret.Get(0).(func(message.TxnContext) message.MutableMessage); ok {
		r0 = rf(txnCtx)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(message.MutableMessage)
		}
	}

	return r0
}

// MockMutableMessage_WithTxnContext_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'WithTxnContext'
type MockMutableMessage_WithTxnContext_Call struct {
	*mock.Call
}

// WithTxnContext is a helper method to define mock.On call
//   - txnCtx message.TxnContext
func (_e *MockMutableMessage_Expecter) WithTxnContext(txnCtx interface{}) *MockMutableMessage_WithTxnContext_Call {
	return &MockMutableMessage_WithTxnContext_Call{Call: _e.mock.On("WithTxnContext", txnCtx)}
}

func (_c *MockMutableMessage_WithTxnContext_Call) Run(run func(txnCtx message.TxnContext)) *MockMutableMessage_WithTxnContext_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(message.TxnContext))
	})
	return _c
}

func (_c *MockMutableMessage_WithTxnContext_Call) Return(_a0 message.MutableMessage) *MockMutableMessage_WithTxnContext_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_WithTxnContext_Call) RunAndReturn(run func(message.TxnContext) message.MutableMessage) *MockMutableMessage_WithTxnContext_Call {
	_c.Call.Return(run)
	return _c
}

// WithWALTerm provides a mock function with given fields: term
func (_m *MockMutableMessage) WithWALTerm(term int64) message.MutableMessage {
	ret := _m.Called(term)

	if len(ret) == 0 {
		panic("no return value specified for WithWALTerm")
	}

	var r0 message.MutableMessage
	if rf, ok := ret.Get(0).(func(int64) message.MutableMessage); ok {
		r0 = rf(term)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(message.MutableMessage)
		}
	}

	return r0
}

// MockMutableMessage_WithWALTerm_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'WithWALTerm'
type MockMutableMessage_WithWALTerm_Call struct {
	*mock.Call
}

// WithWALTerm is a helper method to define mock.On call
//   - term int64
func (_e *MockMutableMessage_Expecter) WithWALTerm(term interface{}) *MockMutableMessage_WithWALTerm_Call {
	return &MockMutableMessage_WithWALTerm_Call{Call: _e.mock.On("WithWALTerm", term)}
}

func (_c *MockMutableMessage_WithWALTerm_Call) Run(run func(term int64)) *MockMutableMessage_WithWALTerm_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(int64))
	})
	return _c
}

func (_c *MockMutableMessage_WithWALTerm_Call) Return(_a0 message.MutableMessage) *MockMutableMessage_WithWALTerm_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockMutableMessage_WithWALTerm_Call) RunAndReturn(run func(int64) message.MutableMessage) *MockMutableMessage_WithWALTerm_Call {
	_c.Call.Return(run)
	return _c
}

// NewMockMutableMessage creates a new instance of MockMutableMessage. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewMockMutableMessage(t interface {
	mock.TestingT
	Cleanup(func())
}) *MockMutableMessage {
	mock := &MockMutableMessage{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
