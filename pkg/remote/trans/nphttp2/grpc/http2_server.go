/*
 *
 * Copyright 2014 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * This file may have been modified by CloudWeGo authors. All CloudWeGo
 * Modifications are Copyright 2021 CloudWeGo Authors.
 */

package grpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/kitex/pkg/remote/codec/protobuf/encoding"
	"github.com/cloudwego/kitex/pkg/remote/transmeta"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/protobuf/proto"

	"github.com/cloudwego/netpoll"

	"github.com/cloudwego/kitex/pkg/gofunc"
	"github.com/cloudwego/kitex/pkg/klog"
	"github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/codes"
	"github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/grpc/grpcframe"
	"github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/metadata"
	"github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/status"
	"github.com/cloudwego/kitex/pkg/utils"
)

const (
	gracefulShutdownCode = http2.ErrCode(1000)
	gracefulShutdownMsg  = "graceful shutdown"
)

var (
	// ErrIllegalHeaderWrite indicates that setting header is illegal because of
	// the stream's state.
	ErrIllegalHeaderWrite       = errors.New("transport: the stream is done or WriteHeader was already called")
	errStatusIllegalHeaderWrite = status.Err(codes.Internal, ErrIllegalHeaderWrite.Error()+triggeredByHandlerSideSuffix)

	// ErrHeaderListSizeLimitViolation indicates that the header list size is larger
	// than the limit set by peer.
	ErrHeaderListSizeLimitViolation       = errors.New("transport: trying to send header list size larger than the limit set by peer")
	errStatusHeaderListSizeLimitViolation = status.Err(codes.Internal, ErrHeaderListSizeLimitViolation.Error()+triggeredByHandlerSideSuffix)

	// errors used for cancelling stream.
	// the code should be codes.Canceled coz it's NOT returned from remote
	errConnectionEOF      = status.Err(codes.Canceled, "transport: connection EOF"+triggeredByRemoteServiceSuffix)
	errMaxStreamsExceeded = status.Err(codes.Canceled, "transport: max streams exceeded"+triggeredByRemoteServiceSuffix)
	errNotReachable       = status.Err(codes.Canceled, "transport: server not reachable"+triggeredByRemoteServiceSuffix)
	errMaxAgeClosing      = status.Err(codes.Canceled, "transport: closing server transport due to maximum connection age"+triggeredByRemoteServiceSuffix)
	errIdleClosing        = status.Err(codes.Canceled, "transport: closing server transport due to idleness"+triggeredByRemoteServiceSuffix)

	errGracefulShutdown = status.Err(codes.Unavailable, gracefulShutdownMsg)
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// http2Server implements the ServerTransport interface with HTTP2.
type http2Server struct {
	lastRead    int64
	ctx         context.Context
	done        chan struct{}
	conn        net.Conn
	loopy       *loopyWriter
	readerDone  chan struct{} // sync point to enable testing.
	writerDone  chan struct{} // denote that the loopyWriter has stopped.
	remoteAddr  net.Addr
	localAddr   net.Addr
	maxStreamID uint32 // max stream ID ever seen
	framer      *framer
	// The max number of concurrent streams.
	maxStreams uint32
	// controlBuf delivers all the control related tasks (e.g., window
	// updates, reset streams, and various settings) to the controller.
	controlBuf *controlBuffer
	fc         *trInFlow
	// Keepalive and max-age parameters for the server.
	kp ServerKeepalive
	// Keepalive enforcement policy.
	kep EnforcementPolicy
	// The time instance last ping was received.
	lastPingAt time.Time
	// Number of times the client has violated keepalive ping policy so far.
	pingStrikes uint8
	// Flag to signify that number of ping strikes should be reset to 0.
	// This is set whenever data or header frames are sent.
	// 1 means yes.
	resetPingStrikes      uint32 // Accessed atomically.
	initialWindowSize     int32
	bdpEst                *bdpEstimator
	maxSendHeaderListSize *uint32

	mu sync.Mutex // guard the following
	// drainChan is initialized when drain(...) is called the first time.
	// After which the server writes out the first GoAway(with ID 2^31-1) frame.
	// Then an independent goroutine will be launched to later send the second GoAway.
	// During this time we don't want to write another first GoAway(with ID 2^31 -1) frame.
	// Thus call to drain(...) will be a no-op if drainChan is already initialized since draining is
	// already underway.
	drainChan chan struct{}
	// denote that drainChan has been closed to prevent remote side sending more ping Ack with goAway
	drainChanClosed bool
	state           transportState
	activeStreams   map[uint32]*Stream
	// idle is the time instant when the connection went idle.
	// This is either the beginning of the connection or when the number of
	// RPCs go down to 0.
	// When the connection is busy, this value is set to 0.
	idle time.Time

	bufferPool *bufferPool
}

// newHTTP2Server constructs a ServerTransport based on HTTP2. ConnectionError is
// returned if something goes wrong.
func newHTTP2Server(ctx context.Context, conn net.Conn, config *ServerConfig) (_ ServerTransport, err error) {
	maxHeaderListSize := defaultServerMaxHeaderListSize
	if config.MaxHeaderListSize != nil {
		maxHeaderListSize = *config.MaxHeaderListSize
	}

	framer := newFramer(conn, config.WriteBufferSize, config.ReadBufferSize, maxHeaderListSize)
	// Send initial settings as connection preface to client.
	isettings := []http2.Setting{{
		ID:  http2.SettingMaxFrameSize,
		Val: http2MaxFrameLen,
	}}

	// 0 is permitted in the HTTP2 spec.
	maxStreams := config.MaxStreams
	if maxStreams == 0 {
		maxStreams = math.MaxUint32
	} else {
		isettings = append(isettings, http2.Setting{
			ID:  http2.SettingMaxConcurrentStreams,
			Val: maxStreams,
		})
	}

	dynamicWindow := true
	iwz := initialWindowSize
	if config.InitialWindowSize >= defaultWindowSize {
		iwz = config.InitialWindowSize
		dynamicWindow = false

		isettings = append(isettings, http2.Setting{
			ID:  http2.SettingInitialWindowSize,
			Val: iwz,
		})
	}
	icwz := initialWindowSize
	if config.InitialConnWindowSize >= defaultWindowSize {
		icwz = config.InitialConnWindowSize
		dynamicWindow = false
	}
	if config.MaxHeaderListSize != nil {
		isettings = append(isettings, http2.Setting{
			ID:  http2.SettingMaxHeaderListSize,
			Val: *config.MaxHeaderListSize,
		})
	}

	if err := framer.WriteSettings(isettings...); err != nil {
		return nil, connectionErrorf(false, err, "transport: %v", err)
	}

	// Adjust the connection flow control window if needed.
	if icwz > defaultWindowSize {
		if delta := icwz - defaultWindowSize; delta > 0 {
			if err := framer.WriteWindowUpdate(0, delta); err != nil {
				return nil, connectionErrorf(false, err, "transport: %v", err)
			}
		}
	}
	kp := config.KeepaliveParams
	if kp.MaxConnectionIdle == 0 {
		kp.MaxConnectionIdle = defaultMaxConnectionIdle
	}
	if kp.MaxConnectionAge == 0 {
		kp.MaxConnectionAge = defaultMaxConnectionAge
	}
	if kp.MaxConnectionAgeGrace == 0 {
		kp.MaxConnectionAgeGrace = defaultMaxConnectionAgeGrace
	}
	if kp.Time == 0 {
		kp.Time = defaultServerKeepaliveTime
	}
	if kp.Timeout == 0 {
		kp.Timeout = defaultServerKeepaliveTimeout
	}
	kep := config.KeepaliveEnforcementPolicy
	if kep.MinTime == 0 {
		kep.MinTime = defaultKeepalivePolicyMinTime
	}

	done := make(chan struct{})
	t := &http2Server{
		ctx:               ctx,
		done:              done,
		conn:              conn,
		remoteAddr:        conn.RemoteAddr(),
		localAddr:         conn.LocalAddr(),
		framer:            framer,
		readerDone:        make(chan struct{}),
		writerDone:        make(chan struct{}),
		maxStreams:        math.MaxUint32,
		fc:                &trInFlow{limit: icwz},
		state:             reachable,
		activeStreams:     make(map[uint32]*Stream),
		kp:                kp,
		kep:               kep,
		idle:              time.Now(),
		initialWindowSize: int32(iwz),
		bufferPool:        newBufferPool(),
	}
	t.controlBuf = newControlBuffer(t.done)
	if dynamicWindow {
		t.bdpEst = &bdpEstimator{
			bdp:               initialWindowSize,
			updateFlowControl: t.updateFlowControl,
		}
	}

	t.framer.writer.Flush()

	defer func() {
		if err != nil {
			t.closeWithErr(err)
		}
	}()

	// Check the validity of client preface.
	preface := make([]byte, len(ClientPreface))
	if _, err := io.ReadFull(t.conn, preface); err != nil {
		// In deployments where a gRPC server runs behind a cloud load balancer
		// which performs regular TCP level health checks, the connection is
		// closed immediately by the latter.  Returning io.EOF here allows the
		// grpc server implementation to recognize this scenario and suppress
		// logging to reduce spam.
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, connectionErrorf(false, err, "transport: http2Server.HandleStreams failed to receive the preface from client: %v", err)
	}
	if !bytes.Equal(preface, ClientPreface) {
		return nil, connectionErrorf(false, nil, "transport: http2Server.HandleStreams received bogus greeting from client: %q", preface)
	}

	frame, err := t.framer.ReadFrame()
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return nil, err
	}
	if err != nil {
		return nil, connectionErrorf(false, err, "transport: http2Server.HandleStreams failed to read initial settings frame: %v", err)
	}
	atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
	sf, ok := frame.(*grpcframe.SettingsFrame)
	if !ok {
		return nil, connectionErrorf(false, nil, "transport: http2Server.HandleStreams saw invalid preface type %T from client", frame)
	}
	t.handleSettings(sf)

	gofunc.RecoverGoFuncWithInfo(ctx, func() {
		t.loopy = newLoopyWriter(serverSide, t.framer, t.controlBuf, t.bdpEst)
		t.loopy.ssGoAwayHandler = t.outgoingGoAwayHandler
		runErr := t.loopy.run(conn.RemoteAddr().String())
		if runErr != nil {
			klog.CtxErrorf(ctx, "KITEX: grpc server loopyWriter.run returning, error=%v", runErr)
		}
		t.conn.Close()
		close(t.writerDone)
	}, gofunc.NewBasicInfo("", conn.RemoteAddr().String()))

	gofunc.RecoverGoFuncWithInfo(ctx, t.keepalive, gofunc.NewBasicInfo("", conn.RemoteAddr().String()))
	return t, nil
}

// operateHeaders takes action on the decoded headers. Returns an error if fatal
// error encountered and transport needs to close, otherwise returns nil.
func (t *http2Server) operateHeaders(frame *grpcframe.MetaHeadersFrame, handle func(*Stream), traceCtx func(context.Context, string) context.Context) error {
	streamID := frame.Header().StreamID
	state := &decodeState{
		serverSide: true,
	}
	if err := state.decodeHeader(frame); err != nil {
		if se, ok := status.FromError(err); ok {
			rstCode := statusCodeConvTab[se.Code()]
			klog.CtxInfof(t.ctx, "KITEX: http2Server.operateHeaders failed to decode header frame, err=%v, rstCode: %d"+sendRSTStreamFrameSuffix, err, rstCode)
			t.controlBuf.put(&cleanupStream{
				streamID: streamID,
				rst:      true,
				rstCode:  rstCode,
				onWrite:  func() {},
			})
		}
		return nil
	}

	buf := newRecvBuffer()
	s := &Stream{
		id:             streamID,
		st:             t,
		buf:            buf,
		fc:             &inFlow{limit: uint32(t.initialWindowSize)},
		recvCompress:   state.data.encoding,
		sendCompress:   state.data.acceptEncoding,
		method:         state.data.method,
		contentSubtype: state.data.contentSubtype,
	}
	if frame.StreamEnded() {
		// s is just created by the caller. No lock needed.
		s.state = streamReadDone
	}
	var cancel context.CancelFunc
	if state.data.timeoutSet {
		s.ctx, cancel = context.WithTimeout(t.ctx, state.data.timeout)
	} else {
		s.ctx, cancel = context.WithCancel(t.ctx)
	}
	s.ctx, s.cancel = newContextWithCancelReason(s.ctx, cancel)
	// Attach the received metadata to the context.
	if len(state.data.mdata) > 0 {
		s.ctx = metadata.NewIncomingContext(s.ctx, state.data.mdata)
		// retrieve source service from metadata
		vals := state.data.mdata[transmeta.HTTPSourceService]
		if len(vals) > 0 {
			s.sourceService = vals[0]
		}
	}

	t.mu.Lock()
	if t.state != reachable {
		t.mu.Unlock()
		s.cancel(errNotReachable)
		return nil
	}
	if uint32(len(t.activeStreams)) >= t.maxStreams {
		t.mu.Unlock()
		klog.CtxInfof(t.ctx, "KITEX: http2Server.operateHeaders failed to create stream, rstCode: %d"+sendRSTStreamFrameSuffix, http2.ErrCodeRefusedStream)
		t.controlBuf.put(&cleanupStream{
			streamID: streamID,
			rst:      true,
			rstCode:  http2.ErrCodeRefusedStream,
			onWrite:  func() {},
		})
		s.cancel(errMaxStreamsExceeded)
		return nil
	}
	if streamID%2 != 1 || streamID <= t.maxStreamID {
		t.mu.Unlock()
		return fmt.Errorf("received an illegal stream id: %v. headers frame: %+v", streamID, frame)
	}
	t.maxStreamID = streamID
	t.activeStreams[streamID] = s
	if len(t.activeStreams) == 1 {
		t.idle = time.Time{}
	}
	t.mu.Unlock()
	s.requestRead = func(n int) {
		t.adjustWindow(s, uint32(n))
	}
	s.ctx = traceCtx(s.ctx, s.method)
	s.ctxDone = s.ctx.Done()
	s.wq = newWriteQuota(defaultWriteQuota, s.ctxDone)
	s.trReader = &transportReader{
		reader: &recvBufferReader{
			ctx:        s.ctx,
			ctxDone:    s.ctxDone,
			recv:       s.buf,
			freeBuffer: t.bufferPool.put,
		},
		windowHandler: func(n int) {
			t.updateWindow(s, uint32(n))
		},
	}
	// Register the stream with loopy.
	t.controlBuf.put(&registerStream{
		streamID: s.id,
		wq:       s.wq,
	})
	handle(s)
	return nil
}

// HandleStreams receives incoming streams using the given handler. This is
// typically run in a separate goroutine.
// traceCtx attaches trace to ctx and returns the new context.
func (t *http2Server) HandleStreams(handle func(*Stream), traceCtx func(context.Context, string) context.Context) {
	defer close(t.readerDone)
	for {
		t.controlBuf.throttle()
		frame, err := t.framer.ReadFrame()
		atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
		if err != nil {
			if se, ok := err.(http2.StreamError); ok {
				klog.CtxWarnf(t.ctx, "transport: http2Server.HandleStreams encountered http2.StreamError: %v", se)
				t.mu.Lock()
				s := t.activeStreams[se.StreamID]
				t.mu.Unlock()
				if s != nil {
					klog.CtxInfof(s.ctx, "KITEX: http2Server.HandleStreams encountered http2.StreamError, err=%v, rstCode: %d"+sendRSTStreamFrameSuffix, err, se.Code)
					t.closeStream(s, status.Errorf(codes.Canceled, "transport: ReadFrame encountered http2.StreamError: %v [triggered by %s]", err, s.sourceService), true, se.Code, false)
				} else {
					klog.CtxInfof(t.ctx, "KITEX: http2Server.HandleStreams failed to ReadFrame, err=%v, rstCode: %d"+sendRSTStreamFrameSuffix, err, se.Code)
					t.controlBuf.put(&cleanupStream{
						streamID: se.StreamID,
						rst:      true,
						rstCode:  se.Code,
						onWrite:  func() {},
					})
				}
				continue
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF || errors.Is(err, netpoll.ErrEOF) {
				t.closeWithErr(errConnectionEOF)
				return
			}
			klog.CtxWarnf(t.ctx, "transport: http2Server.HandleStreams failed to read frame: %v", err)
			t.closeWithErr(status.Errorf(codes.Canceled, "transport: ReadFrame encountered err: %v"+triggeredByRemoteServiceSuffix, err))
			return
		}
		switch frame := frame.(type) {
		case *grpcframe.MetaHeadersFrame:
			if err := t.operateHeaders(frame, handle, traceCtx); err != nil {
				klog.CtxErrorf(t.ctx, "transport: http2Server.HandleStreams fatal err: %v", err)
				t.closeWithErr(err)
				break
			}
		case *grpcframe.DataFrame:
			t.handleData(frame)
		case *http2.RSTStreamFrame:
			t.handleRSTStream(frame)
		case *grpcframe.SettingsFrame:
			t.handleSettings(frame)
		case *http2.PingFrame:
			t.handlePing(frame)
		case *http2.WindowUpdateFrame:
			t.handleWindowUpdate(frame)
		case *grpcframe.GoAwayFrame:
			// TODO: Handle GoAway from the client appropriately.
		default:
			klog.CtxErrorf(t.ctx, "transport: http2Server.HandleStreams found unhandled frame type %v.", frame)
		}
		t.framer.reader.Release()
	}
}

func (t *http2Server) getStream(f http2.Frame) (*Stream, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.activeStreams == nil {
		// The transport is closing.
		return nil, false
	}
	s, ok := t.activeStreams[f.Header().StreamID]
	if !ok {
		// The stream is already done.
		return nil, false
	}
	return s, true
}

// adjustWindow sends out extra window update over the initial window size
// of stream if the application is requesting data larger in size than
// the window.
func (t *http2Server) adjustWindow(s *Stream, n uint32) {
	if w := s.fc.maybeAdjust(n); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{streamID: s.id, increment: w})
	}
}

// updateFlowControl updates the incoming flow control windows
// for the transport and the stream based on the current bdp
// estimation.
func (t *http2Server) updateFlowControl(n uint32) {
	t.mu.Lock()
	for _, s := range t.activeStreams {
		s.fc.newLimit(n)
	}
	t.initialWindowSize = int32(n)
	t.mu.Unlock()
	t.controlBuf.put(&outgoingWindowUpdate{
		streamID:  0,
		increment: t.fc.newLimit(n),
	})
	t.controlBuf.put(&outgoingSettings{
		ss: []http2.Setting{
			{
				ID:  http2.SettingInitialWindowSize,
				Val: n,
			},
		},
	})
}

// updateWindow adjusts the inbound quota for the stream and the transport.
// Window updates will deliver to the controller for sending when
// the cumulative quota exceeds the corresponding threshold.
func (t *http2Server) updateWindow(s *Stream, n uint32) {
	if w := s.fc.onRead(n); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{
			streamID:  s.id,
			increment: w,
		})
	}
}

func (t *http2Server) handleData(f *grpcframe.DataFrame) {
	size := f.Header().Length
	var sendBDPPing bool
	if t.bdpEst != nil {
		sendBDPPing = t.bdpEst.add(size)
	}
	// Decouple connection's flow control from application's read.
	// An update on connection's flow control should not depend on
	// whether user application has read the data or not. Such a
	// restriction is already imposed on the stream's flow control,
	// and therefore the sender will be blocked anyways.
	// Decoupling the connection flow control will prevent other
	// active(fast) streams from starving in presence of slow or
	// inactive streams.
	if w := t.fc.onData(size); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{
			streamID:  0,
			increment: w,
		})
	}
	if sendBDPPing {
		// Avoid excessive ping detection (e.g. in an L7 proxy)
		// by sending a window update prior to the BDP ping.
		if w := t.fc.reset(); w > 0 {
			t.controlBuf.put(&outgoingWindowUpdate{
				streamID:  0,
				increment: w,
			})
		}
		t.controlBuf.put(bdpPing)
	}
	// Select the right stream to dispatch.
	s, ok := t.getStream(f)
	if !ok {
		return
	}
	if size > 0 {
		if err := s.fc.onData(size); err != nil {
			klog.CtxInfof(s.ctx, "KITEX: http2Server.handleData inflow control err: %v, rstCode: %d"+sendRSTStreamFrameSuffix, err, http2.ErrCodeFlowControl)
			t.closeStream(s, status.Errorf(codes.Canceled, "transport: inflow control err: %v [triggered by %s]", err, s.sourceService), true, http2.ErrCodeFlowControl, false)
			return
		}
		if f.Header().Flags.Has(http2.FlagDataPadded) {
			if w := s.fc.onRead(size - uint32(len(f.Data()))); w > 0 {
				t.controlBuf.put(&outgoingWindowUpdate{s.id, w})
			}
		}
		// TODO(bradfitz, zhaoq): A copy is required here because there is no
		// guarantee f.Data() is consumed before the arrival of next frame.
		// Can this copy be eliminated?
		if len(f.Data()) > 0 {
			buffer := t.bufferPool.get()
			buffer.Reset()
			buffer.Write(f.Data())
			s.write(recvMsg{buffer: buffer})
		}
	}
	if f.Header().Flags.Has(http2.FlagDataEndStream) {
		// Received the end of stream from the client.
		s.compareAndSwapState(streamActive, streamReadDone)
		s.write(recvMsg{err: io.EOF})
	}
}

func (t *http2Server) handleRSTStream(f *http2.RSTStreamFrame) {
	// If the stream is not deleted from the transport's active streams map, then do a regular close stream.
	if s, ok := t.getStream(f); ok {
		if f.ErrCode == gracefulShutdownCode {
			t.closeStream(s, errGracefulShutdown, false, 0, false)
		} else {
			t.closeStream(s, status.Errorf(codes.Canceled, "transport: RSTStream Frame received with error code: %v [triggered by %s]", f.ErrCode, s.sourceService), false, 0, false)
		}
		return
	}
	// If the stream is already deleted from the active streams map, then put a cleanupStream item into controlbuf to delete the stream from loopy writer's established streams map.
	t.controlBuf.put(&cleanupStream{
		streamID: f.Header().StreamID,
		rst:      false,
		rstCode:  0,
		onWrite:  func() {},
	})
}

func (t *http2Server) handleSettings(f *grpcframe.SettingsFrame) {
	if f.IsAck() {
		return
	}
	var ss []http2.Setting
	var updateFuncs []func()
	f.ForeachSetting(func(s http2.Setting) error {
		switch s.ID {
		case http2.SettingMaxHeaderListSize:
			updateFuncs = append(updateFuncs, func() {
				t.maxSendHeaderListSize = new(uint32)
				*t.maxSendHeaderListSize = s.Val
			})
		default:
			ss = append(ss, s)
		}
		return nil
	})
	t.controlBuf.executeAndPut(func(interface{}) bool {
		for _, f := range updateFuncs {
			f()
		}
		return true
	}, &incomingSettings{
		ss: ss,
	})
}

const (
	maxPingStrikes     = 2
	defaultPingTimeout = 2 * time.Hour
)

func (t *http2Server) handlePing(f *http2.PingFrame) {
	if f.IsAck() {
		if f.Data == goAwayPing.data {
			t.mu.Lock()
			if !t.drainChanClosed {
				close(t.drainChan)
				t.drainChanClosed = true
			}
			t.mu.Unlock()
		}
		// Maybe it's a BDP ping.
		if t.bdpEst != nil {
			t.bdpEst.calculate(f.Data)
		}
		return
	}
	pingAck := &ping{ack: true}
	copy(pingAck.data[:], f.Data[:])
	t.controlBuf.put(pingAck)

	now := time.Now()
	defer func() {
		t.lastPingAt = now
	}()
	// A reset ping strikes means that we don't need to check for policy
	// violation for this ping and the pingStrikes counter should be set
	// to 0.
	if atomic.CompareAndSwapUint32(&t.resetPingStrikes, 1, 0) {
		t.pingStrikes = 0
		return
	}
	t.mu.Lock()
	ns := len(t.activeStreams)
	t.mu.Unlock()
	if ns < 1 && !t.kep.PermitWithoutStream {
		// Keepalive shouldn't be active thus, this new ping should
		// have come after at least defaultPingTimeout.
		if t.lastPingAt.Add(defaultPingTimeout).After(now) {
			t.pingStrikes++
		}
	} else {
		// Check if keepalive policy is respected.
		if t.lastPingAt.Add(t.kep.MinTime).After(now) {
			t.pingStrikes++
		}
	}

	if t.pingStrikes > maxPingStrikes {
		// Send goaway and close the connection.
		klog.CtxErrorf(t.ctx, "transport: Got too many pings from the client, closing the connection.")
		t.controlBuf.put(&goAway{code: http2.ErrCodeEnhanceYourCalm, debugData: []byte("too_many_pings"), closeConn: true})
	}
}

func (t *http2Server) handleWindowUpdate(f *http2.WindowUpdateFrame) {
	t.controlBuf.put(&incomingWindowUpdate{
		streamID:  f.Header().StreamID,
		increment: f.Increment,
	})
}

func appendHeaderFieldsFromMD(headerFields []hpack.HeaderField, md metadata.MD) []hpack.HeaderField {
	for k, vv := range md {
		if isReservedHeader(k) {
			// Clients don't tolerate reading restricted headers after some non restricted ones were sent.
			continue
		}
		for _, v := range vv {
			headerFields = append(headerFields, hpack.HeaderField{Name: k, Value: encodeMetadataHeader(k, v)})
		}
	}
	return headerFields
}

func (t *http2Server) checkForHeaderListSize(it interface{}) bool {
	if t.maxSendHeaderListSize == nil {
		return true
	}
	hdrFrame := it.(*headerFrame)
	var sz int64
	for _, f := range hdrFrame.hf {
		if sz += int64(f.Size()); sz > int64(*t.maxSendHeaderListSize) {
			klog.CtxErrorf(t.ctx, "header list size to send violates the maximum size (%d bytes) set by client", *t.maxSendHeaderListSize)
			return false
		}
	}
	return true
}

// WriteHeader sends the header metadata md back to the client.
func (t *http2Server) WriteHeader(s *Stream, md metadata.MD) error {
	if s.updateHeaderSent() {
		return errStatusIllegalHeaderWrite
	}
	if s.getState() == streamDone {
		return ContextErr(s.ctx.Err())
	}
	s.hdrMu.Lock()
	if md.Len() > 0 {
		if s.header.Len() > 0 {
			s.header = metadata.AppendMD(s.header, md)
		} else {
			s.header = md
		}
	}
	if err := t.writeHeaderLocked(s); err != nil {
		s.hdrMu.Unlock()
		return err
	}
	s.hdrMu.Unlock()
	return nil
}

func (t *http2Server) setResetPingStrikes() {
	// NOTE: if you're going to change this func
	// update `resetPingStrikes` logic of `dataFrame` as well
	atomic.StoreUint32(&t.resetPingStrikes, 1)
}

func (t *http2Server) writeHeaderLocked(s *Stream) error {
	// first and create a slice of that exact size.
	headerFields := make([]hpack.HeaderField, 0, 3+s.header.Len()) // at least :status, content-type will be there if none else.
	headerFields = append(headerFields, hpack.HeaderField{Name: ":status", Value: "200"})
	headerFields = append(headerFields, hpack.HeaderField{Name: "content-type", Value: contentType(s.contentSubtype)})
	sendCompress := encoding.FindCompressorName(s.sendCompress)
	if sendCompress != "" {
		headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-encoding", Value: sendCompress})
	}
	headerFields = appendHeaderFieldsFromMD(headerFields, s.header)
	success, err := t.controlBuf.executeAndPut(t.checkForHeaderListSize, &headerFrame{
		streamID:  s.id,
		hf:        headerFields,
		endStream: false,
		onWrite:   t.setResetPingStrikes,
	})
	if !success {
		if err != nil {
			return err
		}
		klog.CtxInfof(s.ctx, "KITEX: http2Server.writeHeaderLocked checkForHeaderListSize failed, rstCode: %d"+sendRSTStreamFrameSuffix, http2.ErrCodeInternal)
		t.closeStream(s, errStatusHeaderListSizeLimitViolation, true, http2.ErrCodeInternal, false)
		return errStatusHeaderListSizeLimitViolation
	}
	return nil
}

// WriteStatus sends stream status to the client and terminates the stream.
// There is no further I/O operations being able to perform on this stream.
// TODO(zhaoq): Now it indicates the end of entire stream. Revisit if early
// OK is adopted.
func (t *http2Server) WriteStatus(s *Stream, st *status.Status) error {
	if s.getState() == streamDone {
		return nil
	}
	s.hdrMu.Lock()
	// TODO(mmukhi): Benchmark if the performance gets better if count the metadata and other header fields
	// first and create a slice of that exact size.
	headerFields := make([]hpack.HeaderField, 0, 2) // grpc-status and grpc-message will be there if none else.
	if !s.updateHeaderSent() {                      // No headers have been sent.
		if len(s.header) > 0 { // Send a separate header frame.
			if err := t.writeHeaderLocked(s); err != nil {
				s.hdrMu.Unlock()
				return err
			}
		} else { // Send a trailer only response.
			headerFields = append(headerFields, hpack.HeaderField{Name: ":status", Value: "200"})
			headerFields = append(headerFields, hpack.HeaderField{Name: "content-type", Value: contentType(s.contentSubtype)})
		}
	}
	headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-status", Value: strconv.Itoa(int(st.Code()))})
	headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-message", Value: encodeGrpcMessage(st.Message())})
	if bizStatusErr := s.BizStatusErr(); bizStatusErr != nil {
		headerFields = append(headerFields, hpack.HeaderField{Name: "biz-status", Value: strconv.Itoa(int(bizStatusErr.BizStatusCode()))})
		if len(bizStatusErr.BizExtra()) != 0 {
			value, _ := utils.Map2JSONStr(bizStatusErr.BizExtra())
			headerFields = append(headerFields, hpack.HeaderField{Name: "biz-extra", Value: value})
		}
	}

	if p := st.Proto(); p != nil && len(p.Details) > 0 {
		stBytes, err := proto.Marshal(p)
		if err != nil {
			// TODO: return error instead, when callers are able to handle it.
			klog.CtxErrorf(t.ctx, "transport: failed to marshal rpc status: %v, error: %v", p, err)
		} else {
			headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-status-details-bin", Value: encodeBinHeader(stBytes)})
		}
	}

	// Attach the trailer metadata.
	headerFields = appendHeaderFieldsFromMD(headerFields, s.trailer)
	trailingHeader := &headerFrame{
		streamID:  s.id,
		hf:        headerFields,
		endStream: true,
		onWrite:   t.setResetPingStrikes,
	}
	s.hdrMu.Unlock()
	success, err := t.controlBuf.execute(t.checkForHeaderListSize, trailingHeader)
	if !success {
		if err != nil {
			return err
		}
		klog.CtxInfof(s.ctx, "KITEX: http2Server.WriteStatus checkForHeaderListSize failed, rstCode: %d"+sendRSTStreamFrameSuffix, http2.ErrCodeInternal)
		t.closeStream(s, errStatusHeaderListSizeLimitViolation, true, http2.ErrCodeInternal, false)
		return errStatusHeaderListSizeLimitViolation
	}
	// Send a RST_STREAM after the trailers if the client has not already half-closed.
	rst := s.getState() == streamActive
	t.finishStream(s, rst, http2.ErrCodeNo, trailingHeader, true)
	return nil
}

// Write converts the data into HTTP2 data frame and sends it out. Non-nil error
// is returns if it fails (e.g., framing error, transport error).
func (t *http2Server) Write(s *Stream, hdr, data []byte, opts *Options) error {
	if !s.isHeaderSent() { // Headers haven't been written yet.
		if err := t.WriteHeader(s, nil); err != nil {
			return err
		}
	} else {
		// Writing headers checks for this condition.
		if s.getState() == streamDone {
			return ContextErr(s.ctx.Err())
		}
	}
	df := newDataFrame()
	df.streamID = s.id
	df.h = hdr
	df.d = data
	df.originH = df.h
	df.originD = df.d
	df.resetPingStrikes = &t.resetPingStrikes
	if err := s.wq.get(int32(len(hdr) + len(data))); err != nil {
		return ContextErr(s.ctx.Err())
	}
	return t.controlBuf.put(df)
}

// keepalive running in a separate goroutine does the following:
// 1. Gracefully closes an idle connection after a duration of keepalive.MaxConnectionIdle.
// 2. Gracefully closes any connection after a duration of keepalive.MaxConnectionAge.
// 3. Forcibly closes a connection after an additive period of keepalive.MaxConnectionAgeGrace over keepalive.MaxConnectionAge.
// 4. Makes sure a connection is alive by sending pings with a frequency of keepalive.Time and closes a non-responsive connection
// after an additional duration of keepalive.Timeout.
func (t *http2Server) keepalive() {
	p := &ping{}
	// True iff a ping has been sent, and no data has been received since then.
	outstandingPing := false
	// Amount of time remaining before which we should receive an ACK for the
	// last sent ping.
	kpTimeoutLeft := time.Duration(0)
	// Records the last value of t.lastRead before we go block on the timer.
	// This is required to check for read activity since then.
	prevNano := time.Now().UnixNano()
	// Initialize the different timers to their default values.
	idleTimer := time.NewTimer(t.kp.MaxConnectionIdle)
	ageTimer := time.NewTimer(t.kp.MaxConnectionAge)
	kpTimer := time.NewTimer(t.kp.Time)
	defer func() {
		// We need to drain the underlying channel in these timers after a call
		// to Stop(), only if we are interested in resetting them. Clearly we
		// are not interested in resetting them here.
		idleTimer.Stop()
		ageTimer.Stop()
		kpTimer.Stop()
	}()

	for {
		select {
		case <-idleTimer.C:
			t.mu.Lock()
			idle := t.idle
			if idle.IsZero() { // The connection is non-idle.
				t.mu.Unlock()
				idleTimer.Reset(t.kp.MaxConnectionIdle)
				continue
			}
			val := t.kp.MaxConnectionIdle - time.Since(idle)
			t.mu.Unlock()
			if val <= 0 {
				// The connection has been idle for a duration of keepalive.MaxConnectionIdle or more.
				// Gracefully close the connection.
				t.drain(http2.ErrCodeNo, []byte("idleTimeout"))
				return
			}
			idleTimer.Reset(val)
		case <-ageTimer.C:
			t.drain(http2.ErrCodeNo, []byte("ageTimeout"))
			ageTimer.Reset(t.kp.MaxConnectionAgeGrace)
			select {
			case <-ageTimer.C:
				// Close the connection after grace period.
				klog.Infof("transport: closing server transport due to maximum connection age.")
				t.closeWithErr(errMaxAgeClosing)
			case <-t.done:
			}
			return
		case <-kpTimer.C:
			lastRead := atomic.LoadInt64(&t.lastRead)
			if lastRead > prevNano {
				// There has been read activity since the last time we were
				// here. Setup the timer to fire at kp.Time seconds from
				// lastRead time and continue.
				outstandingPing = false
				kpTimer.Reset(time.Duration(lastRead) + t.kp.Time - time.Duration(time.Now().UnixNano()))
				prevNano = lastRead
				continue
			}
			if outstandingPing && kpTimeoutLeft <= 0 {
				klog.Infof("transport: closing server transport due to idleness.")
				t.closeWithErr(errIdleClosing)
				return
			}
			if !outstandingPing {
				t.controlBuf.put(p)
				kpTimeoutLeft = t.kp.Timeout
				outstandingPing = true
			}
			// The amount of time to sleep here is the minimum of kp.Time and
			// timeoutLeft. This will ensure that we wait only for kp.Time
			// before sending out the next ping (for cases where the ping is
			// acked).
			sleepDuration := minTime(t.kp.Time, kpTimeoutLeft)
			kpTimeoutLeft -= sleepDuration
			kpTimer.Reset(sleepDuration)
		case <-t.done:
			return
		}
	}
}

// Close starts shutting down the http2Server transport.
// TODO(zhaoq): Now the destruction is not blocked on any pending streams. This
// could cause some resource issue. Revisit this later.
func (t *http2Server) Close() error {
	t.mu.Lock()
	if t.state == closing {
		t.mu.Unlock()
		return nil
	}
	t.state = closing
	streams := t.activeStreams
	t.activeStreams = nil
	t.mu.Unlock()

	finishErr := errGracefulShutdown
	finishCh := make(chan struct{}, 1)
	activeNums := t.rstActiveStreams(streams, finishErr, gracefulShutdownCode, finishCh)
	if activeNums == 0 {
		t.closeLoopyWriter(finishErr)
		return nil
	}
	for {
		select {
		// wait for all the RstStream Frames to be written
		case <-finishCh:
			activeNums--
			if activeNums == 0 {
				t.closeLoopyWriter(finishErr)
				return nil
			}
		// loopyWriter has quited, there is no chance to write the RstStream Frame
		case <-t.writerDone:
			t.closeLoopyWriter(finishErr)
			return nil
		}
	}
}

func (t *http2Server) closeLoopyWriter(err error) {
	t.controlBuf.finish(err)
	close(t.done)
	// make use of loopyWriter.run returning to close the connection
	// there is no need to close the connection manually
	<-t.writerDone
}

// rstActiveStreams sends RSTStream frames to all active streams.
func (t *http2Server) rstActiveStreams(streams map[uint32]*Stream, cancelErr error, rstCode http2.ErrCode, finishCh chan struct{}) (activeStreams int) {
	for _, s := range streams {
		oldState := s.swapState(streamDone)
		if oldState == streamDone {
			// If the stream was already done, continue
			continue
		}
		activeStreams++
		// cancel the downstream
		s.cancel(cancelErr)
		// send RSTStream Frame to the upstream
		t.controlBuf.put(&cleanupStream{
			streamID: s.id,
			rst:      true,
			rstCode:  rstCode,
			onWrite:  func() {},
			onFinishWrite: func() {
				finishCh <- struct{}{}
			},
		})
	}
	return activeStreams
}

func (t *http2Server) closeWithErr(reason error) error {
	t.mu.Lock()
	if t.state == closing {
		t.mu.Unlock()
		return errors.New("transport: Close() was already called")
	}
	t.state = closing
	streams := t.activeStreams
	t.activeStreams = nil
	t.mu.Unlock()
	t.controlBuf.finish(reason)
	close(t.done)
	err := t.conn.Close()

	// Cancel all active streams.
	for _, s := range streams {
		s.cancel(reason)
	}

	return err
}

// deleteStream deletes the stream s from transport's active streams.
func (t *http2Server) deleteStream(s *Stream, eosReceived bool) {
	t.mu.Lock()
	if _, ok := t.activeStreams[s.id]; ok {
		delete(t.activeStreams, s.id)
		if len(t.activeStreams) == 0 {
			t.idle = time.Now()
		}
	}
	t.mu.Unlock()
}

// finishStream closes the stream and puts the trailing headerFrame into controlbuf.
func (t *http2Server) finishStream(s *Stream, rst bool, rstCode http2.ErrCode, hdr *headerFrame, eosReceived bool) {
	oldState := s.swapState(streamDone)
	if oldState == streamDone {
		// If the stream was already done, return.
		return
	}
	s.cancel(nil)

	hdr.cleanup = &cleanupStream{
		streamID: s.id,
		rst:      rst,
		rstCode:  rstCode,
		onWrite: func() {
			t.deleteStream(s, eosReceived)
		},
	}
	t.controlBuf.put(hdr)
}

// closeStream clears the footprint of a stream when the stream is not needed any more.
func (t *http2Server) closeStream(s *Stream, err error, rst bool, rstCode http2.ErrCode, eosReceived bool) {
	// In case stream sending and receiving are invoked in separate
	// goroutines (e.g., bi-directional streaming), cancel needs to be
	// called to interrupt the potential blocking on other goroutines.
	s.cancel(err)
	s.swapState(streamDone)
	t.deleteStream(s, eosReceived)

	t.controlBuf.put(&cleanupStream{
		streamID: s.id,
		rst:      rst,
		rstCode:  rstCode,
		onWrite:  func() {},
	})
}

func (t *http2Server) RemoteAddr() net.Addr {
	return t.remoteAddr
}

func (t *http2Server) LocalAddr() net.Addr {
	return t.localAddr
}

func (t *http2Server) Drain() {
	t.drain(http2.ErrCodeNo, []byte(gracefulShutdownMsg))
}

func (t *http2Server) drain(code http2.ErrCode, debugData []byte) {
	t.mu.Lock()
	if t.drainChan != nil {
		t.mu.Unlock()
		return
	}
	t.drainChan = make(chan struct{})
	t.mu.Unlock()
	// drain successfully
	// should release lock before access controlBuf
	t.controlBuf.put(&goAway{code: code, debugData: debugData, headsUp: true})
}

var goAwayPing = &ping{data: [8]byte{1, 6, 1, 8, 0, 3, 3, 9}}

// Handles outgoing GoAway and returns true if loopy needs to put itself
// in draining mode.
func (t *http2Server) outgoingGoAwayHandler(g *goAway) (bool, error) {
	t.mu.Lock()
	if t.state == closing { // TODO(mmukhi): This seems unnecessary.
		t.mu.Unlock()
		// The transport is closing.
		return false, ErrConnClosing
	}
	sid := t.maxStreamID
	if !g.headsUp {
		// Stop accepting more streams now.
		t.state = draining
		if len(t.activeStreams) == 0 {
			g.closeConn = true
		}
		t.mu.Unlock()
		if err := t.framer.WriteGoAway(sid, g.code, g.debugData); err != nil {
			return false, err
		}
		if g.closeConn {
			// Abruptly close the connection following the GoAway (via
			// loopywriter).  But flush out what's inside the buffer first.
			t.framer.writer.Flush()
			return false, fmt.Errorf("transport: Connection closing")
		}
		return true, nil
	}
	t.mu.Unlock()
	// For a graceful close, send out a GoAway with stream ID of MaxUInt32,
	// Follow that with a ping and wait for the ack to come back or a timer
	// to expire. During this time accept new streams since they might have
	// originated before the GoAway reaches the client.
	// After getting the ack or timer expiration send out another GoAway this
	// time with an ID of the max stream server intends to process.
	if err := t.framer.WriteGoAway(math.MaxUint32, http2.ErrCodeNo, g.debugData); err != nil {
		return false, err
	}
	if err := t.framer.WritePing(false, goAwayPing.data); err != nil {
		return false, err
	}

	gofunc.RecoverGoFuncWithInfo(context.Background(), func() {
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		select {
		case <-t.drainChan:
		case <-timer.C:
		case <-t.done:
			return
		}
		t.controlBuf.put(&goAway{code: g.code, debugData: g.debugData})
	}, gofunc.NewBasicInfo("", t.conn.RemoteAddr().String())) // we should create a new BasicInfo here
	return false, nil
}
