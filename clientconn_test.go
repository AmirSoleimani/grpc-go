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
 */

package grpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"github.com/AmirSoleimani/grpc-go/connectivity"
	"github.com/AmirSoleimani/grpc-go/credentials"
	"github.com/AmirSoleimani/grpc-go/internal/backoff"
	"github.com/AmirSoleimani/grpc-go/internal/envconfig"
	"github.com/AmirSoleimani/grpc-go/internal/transport"
	"github.com/AmirSoleimani/grpc-go/keepalive"
	"github.com/AmirSoleimani/grpc-go/naming"
	"github.com/AmirSoleimani/grpc-go/resolver"
	"github.com/AmirSoleimani/grpc-go/resolver/manual"
	_ "github.com/AmirSoleimani/grpc-go/resolver/passthrough"
	"github.com/AmirSoleimani/grpc-go/testdata"
)

var (
	mutableMinConnectTimeout = time.Second * 20
)

func init() {
	getMinConnectTimeout = func() time.Duration {
		return time.Duration(atomic.LoadInt64((*int64)(&mutableMinConnectTimeout)))
	}
}

func assertState(wantState connectivity.State, cc *ClientConn) (connectivity.State, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var state connectivity.State
	for state = cc.GetState(); state != wantState && cc.WaitForStateChange(ctx, state); state = cc.GetState() {
	}
	return state, state == wantState
}

func (s) TestDialWithMultipleBackendsNotSendingServerPreface(t *testing.T) {
	lis1, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Error while listening. Err: %v", err)
	}
	defer lis1.Close()
	lis1Addr := resolver.Address{Addr: lis1.Addr().String()}
	lis1Done := make(chan struct{})
	// 1st listener accepts the connection and immediately closes it.
	go func() {
		defer close(lis1Done)
		conn, err := lis1.Accept()
		if err != nil {
			t.Errorf("Error while accepting. Err: %v", err)
			return
		}
		conn.Close()
	}()

	lis2, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Error while listening. Err: %v", err)
	}
	defer lis2.Close()
	lis2Done := make(chan struct{})
	lis2Addr := resolver.Address{Addr: lis2.Addr().String()}
	// 2nd listener should get a connection attempt since the first one failed.
	go func() {
		defer close(lis2Done)
		_, err := lis2.Accept() // Closing the client will clean up this conn.
		if err != nil {
			t.Errorf("Error while accepting. Err: %v", err)
			return
		}
	}()

	r, cleanup := manual.GenerateAndRegisterManualResolver()
	defer cleanup()
	r.InitialAddrs([]resolver.Address{lis1Addr, lis2Addr})
	client, err := Dial(r.Scheme()+":///test.server", WithInsecure())
	if err != nil {
		t.Fatalf("Dial failed. Err: %v", err)
	}
	defer client.Close()
	timeout := time.After(5 * time.Second)
	select {
	case <-timeout:
		t.Fatal("timed out waiting for server 1 to finish")
	case <-lis1Done:
	}
	select {
	case <-timeout:
		t.Fatal("timed out waiting for server 2 to finish")
	case <-lis2Done:
	}
}

var allReqHSSettings = []envconfig.RequireHandshakeSetting{
	envconfig.RequireHandshakeOff,
	envconfig.RequireHandshakeOn,
	envconfig.RequireHandshakeHybrid,
}
var reqNoHSSettings = []envconfig.RequireHandshakeSetting{
	envconfig.RequireHandshakeOff,
	envconfig.RequireHandshakeHybrid,
}
var reqHSBeforeSuccess = []envconfig.RequireHandshakeSetting{
	envconfig.RequireHandshakeOn,
	envconfig.RequireHandshakeHybrid,
}

func (s) TestDialWaitsForServerSettings(t *testing.T) {
	// Restore current setting after test.
	old := envconfig.RequireHandshake
	defer func() { envconfig.RequireHandshake = old }()

	// Test with all environment variable settings, which should not impact the
	// test case since WithWaitForHandshake has higher priority.
	for _, setting := range allReqHSSettings {
		envconfig.RequireHandshake = setting
		lis, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatalf("Error while listening. Err: %v", err)
		}
		defer lis.Close()
		done := make(chan struct{})
		sent := make(chan struct{})
		dialDone := make(chan struct{})
		go func() { // Launch the server.
			defer func() {
				close(done)
			}()
			conn, err := lis.Accept()
			if err != nil {
				t.Errorf("Error while accepting. Err: %v", err)
				return
			}
			defer conn.Close()
			// Sleep for a little bit to make sure that Dial on client
			// side blocks until settings are received.
			time.Sleep(100 * time.Millisecond)
			framer := http2.NewFramer(conn, conn)
			close(sent)
			if err := framer.WriteSettings(http2.Setting{}); err != nil {
				t.Errorf("Error while writing settings. Err: %v", err)
				return
			}
			<-dialDone // Close conn only after dial returns.
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client, err := DialContext(ctx, lis.Addr().String(), WithInsecure(), WithWaitForHandshake(), WithBlock())
		close(dialDone)
		if err != nil {
			t.Fatalf("Error while dialing. Err: %v", err)
		}
		defer client.Close()
		select {
		case <-sent:
		default:
			t.Fatalf("Dial returned before server settings were sent")
		}
		<-done
	}
}

func (s) TestDialWaitsForServerSettingsViaEnv(t *testing.T) {
	// Set default behavior and restore current setting after test.
	old := envconfig.RequireHandshake
	envconfig.RequireHandshake = envconfig.RequireHandshakeOn
	defer func() { envconfig.RequireHandshake = old }()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Error while listening. Err: %v", err)
	}
	defer lis.Close()
	done := make(chan struct{})
	sent := make(chan struct{})
	dialDone := make(chan struct{})
	go func() { // Launch the server.
		defer func() {
			close(done)
		}()
		conn, err := lis.Accept()
		if err != nil {
			t.Errorf("Error while accepting. Err: %v", err)
			return
		}
		defer conn.Close()
		// Sleep for a little bit to make sure that Dial on client
		// side blocks until settings are received.
		time.Sleep(100 * time.Millisecond)
		framer := http2.NewFramer(conn, conn)
		close(sent)
		if err := framer.WriteSettings(http2.Setting{}); err != nil {
			t.Errorf("Error while writing settings. Err: %v", err)
			return
		}
		<-dialDone // Close conn only after dial returns.
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := DialContext(ctx, lis.Addr().String(), WithInsecure(), WithBlock())
	close(dialDone)
	if err != nil {
		t.Fatalf("Error while dialing. Err: %v", err)
	}
	defer client.Close()
	select {
	case <-sent:
	default:
		t.Fatalf("Dial returned before server settings were sent")
	}
	<-done
}

func (s) TestDialWaitsForServerSettingsAndFails(t *testing.T) {
	// Restore current setting after test.
	old := envconfig.RequireHandshake
	defer func() { envconfig.RequireHandshake = old }()

	for _, setting := range allReqHSSettings {
		envconfig.RequireHandshake = setting
		lis, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatalf("Error while listening. Err: %v", err)
		}
		done := make(chan struct{})
		numConns := 0
		go func() { // Launch the server.
			defer func() {
				close(done)
			}()
			for {
				conn, err := lis.Accept()
				if err != nil {
					break
				}
				numConns++
				defer conn.Close()
			}
		}()
		cleanup := setMinConnectTimeout(time.Second / 4)
		defer cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		client, err := DialContext(ctx, lis.Addr().String(), WithInsecure(), WithWaitForHandshake(), WithBlock(), withBackoff(noBackoff{}))
		lis.Close()
		if err == nil {
			client.Close()
			t.Fatalf("Unexpected success (err=nil) while dialing")
		}
		if err != context.DeadlineExceeded {
			t.Fatalf("DialContext(_) = %v; want context.DeadlineExceeded", err)
		}
		if numConns < 2 {
			t.Fatalf("dial attempts: %v; want > 1", numConns)
		}
		<-done
	}
}

func (s) TestDialWaitsForServerSettingsViaEnvAndFails(t *testing.T) {
	// Set default behavior and restore current setting after test.
	old := envconfig.RequireHandshake
	envconfig.RequireHandshake = envconfig.RequireHandshakeOn
	defer func() { envconfig.RequireHandshake = old }()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Error while listening. Err: %v", err)
	}
	done := make(chan struct{})
	numConns := 0
	go func() { // Launch the server.
		defer func() {
			close(done)
		}()
		for {
			conn, err := lis.Accept()
			if err != nil {
				break
			}
			numConns++
			defer conn.Close()
		}
	}()
	cleanup := setMinConnectTimeout(time.Second / 4)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	client, err := DialContext(ctx, lis.Addr().String(), WithInsecure(), WithBlock(), withBackoff(noBackoff{}))
	lis.Close()
	if err == nil {
		client.Close()
		t.Fatalf("Unexpected success (err=nil) while dialing")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("DialContext(_) = %v; want context.DeadlineExceeded", err)
	}
	if numConns < 2 {
		t.Fatalf("dial attempts: %v; want > 1", numConns)
	}
	<-done
}

func (s) TestDialDoesNotWaitForServerSettings(t *testing.T) {
	// Restore current setting after test.
	old := envconfig.RequireHandshake
	defer func() { envconfig.RequireHandshake = old }()

	// Test with "off" and "hybrid".
	for _, setting := range reqNoHSSettings {
		envconfig.RequireHandshake = setting
		lis, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatalf("Error while listening. Err: %v", err)
		}
		defer lis.Close()
		done := make(chan struct{})
		dialDone := make(chan struct{})
		go func() { // Launch the server.
			defer func() {
				close(done)
			}()
			conn, err := lis.Accept()
			if err != nil {
				t.Errorf("Error while accepting. Err: %v", err)
				return
			}
			defer conn.Close()
			<-dialDone // Close conn only after dial returns.
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client, err := DialContext(ctx, lis.Addr().String(), WithInsecure(), WithBlock())

		if err != nil {
			t.Fatalf("DialContext returned err =%v; want nil", err)
		}
		defer client.Close()

		if state := client.GetState(); state != connectivity.Ready {
			t.Fatalf("client.GetState() = %v; want connectivity.Ready", state)
		}
		close(dialDone)
		<-done
	}
}

func (s) TestCloseConnectionWhenServerPrefaceNotReceived(t *testing.T) {
	// Restore current setting after test.
	old := envconfig.RequireHandshake
	defer func() { envconfig.RequireHandshake = old }()

	// 1. Client connects to a server that doesn't send preface.
	// 2. After minConnectTimeout(500 ms here), client disconnects and retries.
	// 3. The new server sends its preface.
	// 4. Client doesn't kill the connection this time.
	cleanup := setMinConnectTimeout(time.Millisecond * 500)
	defer cleanup()

	// Test with "on" and "hybrid".
	for _, setting := range reqHSBeforeSuccess {
		envconfig.RequireHandshake = setting

		lis, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatalf("Error while listening. Err: %v", err)
		}
		var (
			conn2 net.Conn
			over  uint32
		)
		defer func() {
			lis.Close()
			// conn2 shouldn't be closed until the client has
			// observed a successful test.
			if conn2 != nil {
				conn2.Close()
			}
		}()
		done := make(chan struct{})
		accepted := make(chan struct{})
		go func() { // Launch the server.
			defer close(done)
			conn1, err := lis.Accept()
			if err != nil {
				t.Errorf("Error while accepting. Err: %v", err)
				return
			}
			defer conn1.Close()
			// Don't send server settings and the client should close the connection and try again.
			conn2, err = lis.Accept() // Accept a reconnection request from client.
			if err != nil {
				t.Errorf("Error while accepting. Err: %v", err)
				return
			}
			close(accepted)
			framer := http2.NewFramer(conn2, conn2)
			if err = framer.WriteSettings(http2.Setting{}); err != nil {
				t.Errorf("Error while writing settings. Err: %v", err)
				return
			}
			b := make([]byte, 8)
			for {
				_, err = conn2.Read(b)
				if err == nil {
					continue
				}
				if atomic.LoadUint32(&over) == 1 {
					// The connection stayed alive for the timer.
					// Success.
					return
				}
				t.Errorf("Unexpected error while reading. Err: %v, want timeout error", err)
				break
			}
		}()
		client, err := Dial(lis.Addr().String(), WithInsecure())
		if err != nil {
			t.Fatalf("Error while dialing. Err: %v", err)
		}
		// wait for connection to be accepted on the server.
		timer := time.NewTimer(time.Second * 10)
		select {
		case <-accepted:
		case <-timer.C:
			t.Fatalf("Client didn't make another connection request in time.")
		}
		// Make sure the connection stays alive for sometime.
		time.Sleep(time.Second)
		atomic.StoreUint32(&over, 1)
		client.Close()
		<-done
	}
}

func (s) TestBackoffWhenNoServerPrefaceReceived(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Error while listening. Err: %v", err)
	}
	defer lis.Close()
	done := make(chan struct{})
	go func() { // Launch the server.
		defer func() {
			close(done)
		}()
		conn, err := lis.Accept() // Accept the connection only to close it immediately.
		if err != nil {
			t.Errorf("Error while accepting. Err: %v", err)
			return
		}
		prevAt := time.Now()
		conn.Close()
		var prevDuration time.Duration
		// Make sure the retry attempts are backed off properly.
		for i := 0; i < 3; i++ {
			conn, err := lis.Accept()
			if err != nil {
				t.Errorf("Error while accepting. Err: %v", err)
				return
			}
			meow := time.Now()
			conn.Close()
			dr := meow.Sub(prevAt)
			if dr <= prevDuration {
				t.Errorf("Client backoff did not increase with retries. Previous duration: %v, current duration: %v", prevDuration, dr)
				return
			}
			prevDuration = dr
			prevAt = meow
		}
	}()
	client, err := Dial(lis.Addr().String(), WithInsecure())
	if err != nil {
		t.Fatalf("Error while dialing. Err: %v", err)
	}
	defer client.Close()
	<-done

}

func (s) TestConnectivityStates(t *testing.T) {
	servers, resolver, cleanup := startServers(t, 2, math.MaxUint32)
	defer cleanup()
	cc, err := Dial("passthrough:///foo.bar.com", WithBalancer(RoundRobin(resolver)), WithInsecure())
	if err != nil {
		t.Fatalf("Dial(\"foo.bar.com\", WithBalancer(_)) = _, %v, want _ <nil>", err)
	}
	defer cc.Close()
	wantState := connectivity.Ready
	if state, ok := assertState(wantState, cc); !ok {
		t.Fatalf("asserState(%s) = %s, false, want %s, true", wantState, state, wantState)
	}
	// Send an update to delete the server connection (tearDown addrConn).
	update := []*naming.Update{
		{
			Op:   naming.Delete,
			Addr: "localhost:" + servers[0].port,
		},
	}
	resolver.w.inject(update)
	wantState = connectivity.TransientFailure
	if state, ok := assertState(wantState, cc); !ok {
		t.Fatalf("asserState(%s) = %s, false, want %s, true", wantState, state, wantState)
	}
	update[0] = &naming.Update{
		Op:   naming.Add,
		Addr: "localhost:" + servers[1].port,
	}
	resolver.w.inject(update)
	wantState = connectivity.Ready
	if state, ok := assertState(wantState, cc); !ok {
		t.Fatalf("asserState(%s) = %s, false, want %s, true", wantState, state, wantState)
	}

}

func (s) TestWithTimeout(t *testing.T) {
	conn, err := Dial("passthrough:///Non-Existent.Server:80", WithTimeout(time.Millisecond), WithBlock(), WithInsecure())
	if err == nil {
		conn.Close()
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("Dial(_, _) = %v, %v, want %v", conn, err, context.DeadlineExceeded)
	}
}

func (s) TestWithTransportCredentialsTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	creds, err := credentials.NewClientTLSFromFile(testdata.Path("ca.pem"), "x.test.youtube.com")
	if err != nil {
		t.Fatalf("Failed to create credentials %v", err)
	}
	conn, err := DialContext(ctx, "passthrough:///Non-Existent.Server:80", WithTransportCredentials(creds), WithBlock())
	if err == nil {
		conn.Close()
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("Dial(_, _) = %v, %v, want %v", conn, err, context.DeadlineExceeded)
	}
}

func (s) TestDefaultAuthority(t *testing.T) {
	target := "Non-Existent.Server:8080"
	conn, err := Dial(target, WithInsecure())
	if err != nil {
		t.Fatalf("Dial(_, _) = _, %v, want _, <nil>", err)
	}
	defer conn.Close()
	if conn.authority != target {
		t.Fatalf("%v.authority = %v, want %v", conn, conn.authority, target)
	}
}

func (s) TestTLSServerNameOverwrite(t *testing.T) {
	overwriteServerName := "over.write.server.name"
	creds, err := credentials.NewClientTLSFromFile(testdata.Path("ca.pem"), overwriteServerName)
	if err != nil {
		t.Fatalf("Failed to create credentials %v", err)
	}
	conn, err := Dial("passthrough:///Non-Existent.Server:80", WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("Dial(_, _) = _, %v, want _, <nil>", err)
	}
	defer conn.Close()
	if conn.authority != overwriteServerName {
		t.Fatalf("%v.authority = %v, want %v", conn, conn.authority, overwriteServerName)
	}
}

func (s) TestWithAuthority(t *testing.T) {
	overwriteServerName := "over.write.server.name"
	conn, err := Dial("passthrough:///Non-Existent.Server:80", WithInsecure(), WithAuthority(overwriteServerName))
	if err != nil {
		t.Fatalf("Dial(_, _) = _, %v, want _, <nil>", err)
	}
	defer conn.Close()
	if conn.authority != overwriteServerName {
		t.Fatalf("%v.authority = %v, want %v", conn, conn.authority, overwriteServerName)
	}
}

func (s) TestWithAuthorityAndTLS(t *testing.T) {
	overwriteServerName := "over.write.server.name"
	creds, err := credentials.NewClientTLSFromFile(testdata.Path("ca.pem"), overwriteServerName)
	if err != nil {
		t.Fatalf("Failed to create credentials %v", err)
	}
	conn, err := Dial("passthrough:///Non-Existent.Server:80", WithTransportCredentials(creds), WithAuthority("no.effect.authority"))
	if err != nil {
		t.Fatalf("Dial(_, _) = _, %v, want _, <nil>", err)
	}
	defer conn.Close()
	if conn.authority != overwriteServerName {
		t.Fatalf("%v.authority = %v, want %v", conn, conn.authority, overwriteServerName)
	}
}

func (s) TestDialContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DialContext(ctx, "Non-Existent.Server:80", WithBlock(), WithInsecure()); err != context.Canceled {
		t.Fatalf("DialContext(%v, _) = _, %v, want _, %v", ctx, err, context.Canceled)
	}
}

type failFastError struct{}

func (failFastError) Error() string   { return "failfast" }
func (failFastError) Temporary() bool { return false }

func (s) TestDialContextFailFast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	failErr := failFastError{}
	dialer := func(string, time.Duration) (net.Conn, error) {
		return nil, failErr
	}

	_, err := DialContext(ctx, "Non-Existent.Server:80", WithBlock(), WithInsecure(), WithDialer(dialer), FailOnNonTempDialError(true))
	if terr, ok := err.(transport.ConnectionError); !ok || terr.Origin() != failErr {
		t.Fatalf("DialContext() = _, %v, want _, %v", err, failErr)
	}
}

// blockingBalancer mimics the behavior of balancers whose initialization takes a long time.
// In this test, reading from blockingBalancer.Notify() blocks forever.
type blockingBalancer struct {
	ch chan []Address
}

func newBlockingBalancer() Balancer {
	return &blockingBalancer{ch: make(chan []Address)}
}
func (b *blockingBalancer) Start(target string, config BalancerConfig) error {
	return nil
}
func (b *blockingBalancer) Up(addr Address) func(error) {
	return nil
}
func (b *blockingBalancer) Get(ctx context.Context, opts BalancerGetOptions) (addr Address, put func(), err error) {
	return Address{}, nil, nil
}
func (b *blockingBalancer) Notify() <-chan []Address {
	return b.ch
}
func (b *blockingBalancer) Close() error {
	close(b.ch)
	return nil
}

func (s) TestDialWithBlockingBalancer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dialDone := make(chan struct{})
	go func() {
		DialContext(ctx, "Non-Existent.Server:80", WithBlock(), WithInsecure(), WithBalancer(newBlockingBalancer()))
		close(dialDone)
	}()
	cancel()
	<-dialDone
}

// securePerRPCCredentials always requires transport security.
type securePerRPCCredentials struct{}

func (c securePerRPCCredentials) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return nil, nil
}

func (c securePerRPCCredentials) RequireTransportSecurity() bool {
	return true
}

func (s) TestCredentialsMisuse(t *testing.T) {
	tlsCreds, err := credentials.NewClientTLSFromFile(testdata.Path("ca.pem"), "x.test.youtube.com")
	if err != nil {
		t.Fatalf("Failed to create authenticator %v", err)
	}
	// Two conflicting credential configurations
	if _, err := Dial("passthrough:///Non-Existent.Server:80", WithTransportCredentials(tlsCreds), WithBlock(), WithInsecure()); err != errCredentialsConflict {
		t.Fatalf("Dial(_, _) = _, %v, want _, %v", err, errCredentialsConflict)
	}
	// security info on insecure connection
	if _, err := Dial("passthrough:///Non-Existent.Server:80", WithPerRPCCredentials(securePerRPCCredentials{}), WithBlock(), WithInsecure()); err != errTransportCredentialsMissing {
		t.Fatalf("Dial(_, _) = _, %v, want _, %v", err, errTransportCredentialsMissing)
	}
}

func (s) TestWithBackoffConfigDefault(t *testing.T) {
	testBackoffConfigSet(t, &DefaultBackoffConfig)
}

func (s) TestWithBackoffConfig(t *testing.T) {
	b := BackoffConfig{MaxDelay: DefaultBackoffConfig.MaxDelay / 2}
	expected := b
	testBackoffConfigSet(t, &expected, WithBackoffConfig(b))
}

func (s) TestWithBackoffMaxDelay(t *testing.T) {
	md := DefaultBackoffConfig.MaxDelay / 2
	expected := BackoffConfig{MaxDelay: md}
	testBackoffConfigSet(t, &expected, WithBackoffMaxDelay(md))
}

func testBackoffConfigSet(t *testing.T, expected *BackoffConfig, opts ...DialOption) {
	opts = append(opts, WithInsecure())
	conn, err := Dial("passthrough:///foo:80", opts...)
	if err != nil {
		t.Fatalf("unexpected error dialing connection: %v", err)
	}
	defer conn.Close()

	if conn.dopts.bs == nil {
		t.Fatalf("backoff config not set")
	}

	actual, ok := conn.dopts.bs.(backoff.Exponential)
	if !ok {
		t.Fatalf("unexpected type of backoff config: %#v", conn.dopts.bs)
	}

	expectedValue := backoff.Exponential{
		MaxDelay: expected.MaxDelay,
	}
	if actual != expectedValue {
		t.Fatalf("unexpected backoff config on connection: %v, want %v", actual, expected)
	}
}

// emptyBalancer returns an empty set of servers.
type emptyBalancer struct {
	ch chan []Address
}

func newEmptyBalancer() Balancer {
	return &emptyBalancer{ch: make(chan []Address, 1)}
}
func (b *emptyBalancer) Start(_ string, _ BalancerConfig) error {
	b.ch <- nil
	return nil
}
func (b *emptyBalancer) Up(_ Address) func(error) {
	return nil
}
func (b *emptyBalancer) Get(_ context.Context, _ BalancerGetOptions) (Address, func(), error) {
	return Address{}, nil, nil
}
func (b *emptyBalancer) Notify() <-chan []Address {
	return b.ch
}
func (b *emptyBalancer) Close() error {
	close(b.ch)
	return nil
}

func (s) TestNonblockingDialWithEmptyBalancer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialDone := make(chan error)
	go func() {
		dialDone <- func() error {
			conn, err := DialContext(ctx, "Non-Existent.Server:80", WithInsecure(), WithBalancer(newEmptyBalancer()))
			if err != nil {
				return err
			}
			return conn.Close()
		}()
	}()
	if err := <-dialDone; err != nil {
		t.Fatalf("unexpected error dialing connection: %s", err)
	}
}

func (s) TestResolverServiceConfigBeforeAddressNotPanic(t *testing.T) {
	r, rcleanup := manual.GenerateAndRegisterManualResolver()
	defer rcleanup()

	cc, err := Dial(r.Scheme()+":///test.server", WithInsecure())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer cc.Close()

	// SwitchBalancer before NewAddress. There was no balancer created, this
	// makes sure we don't call close on nil balancerWrapper.
	r.NewServiceConfig(`{"loadBalancingPolicy": "round_robin"}`) // This should not panic.

	time.Sleep(time.Second) // Sleep to make sure the service config is handled by ClientConn.
}

func (s) TestResolverServiceConfigWhileClosingNotPanic(t *testing.T) {
	for i := 0; i < 10; i++ { // Run this multiple times to make sure it doesn't panic.
		r, rcleanup := manual.GenerateAndRegisterManualResolver()
		defer rcleanup()

		cc, err := Dial(r.Scheme()+":///test.server", WithInsecure())
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}
		// Send a new service config while closing the ClientConn.
		go cc.Close()
		go r.NewServiceConfig(`{"loadBalancingPolicy": "round_robin"}`) // This should not panic.
	}
}

func (s) TestResolverEmptyUpdateNotPanic(t *testing.T) {
	r, rcleanup := manual.GenerateAndRegisterManualResolver()
	defer rcleanup()

	cc, err := Dial(r.Scheme()+":///test.server", WithInsecure())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer cc.Close()

	// This make sure we don't create addrConn with empty address list.
	r.NewAddress([]resolver.Address{}) // This should not panic.

	time.Sleep(time.Second) // Sleep to make sure the service config is handled by ClientConn.
}

func (s) TestClientUpdatesParamsAfterGoAway(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to listen. Err: %v", err)
	}
	defer lis.Close()
	addr := lis.Addr().String()
	s := NewServer()
	go s.Serve(lis)
	defer s.Stop()
	cc, err := Dial(addr, WithBlock(), WithInsecure(), WithKeepaliveParams(keepalive.ClientParameters{
		Time:                50 * time.Millisecond,
		Timeout:             100 * time.Millisecond,
		PermitWithoutStream: true,
	}))
	if err != nil {
		t.Fatalf("Dial(%s, _) = _, %v, want _, <nil>", addr, err)
	}
	defer cc.Close()
	time.Sleep(1 * time.Second)
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	v := cc.mkp.Time
	if v < 100*time.Millisecond {
		t.Fatalf("cc.dopts.copts.Keepalive.Time = %v , want 100ms", v)
	}
}

func (s) TestDisableServiceConfigOption(t *testing.T) {
	r, cleanup := manual.GenerateAndRegisterManualResolver()
	defer cleanup()
	addr := r.Scheme() + ":///non.existent"
	cc, err := Dial(addr, WithInsecure(), WithDisableServiceConfig())
	if err != nil {
		t.Fatalf("Dial(%s, _) = _, %v, want _, <nil>", addr, err)
	}
	defer cc.Close()
	r.NewServiceConfig(`{
    "methodConfig": [
        {
            "name": [
                {
                    "service": "foo",
                    "method": "Bar"
                }
            ],
            "waitForReady": true
        }
    ]
}`)
	time.Sleep(1 * time.Second)
	m := cc.GetMethodConfig("/foo/Bar")
	if m.WaitForReady != nil {
		t.Fatalf("want: method (\"/foo/bar/\") config to be empty, got: %v", m)
	}
}

func (s) TestGetClientConnTarget(t *testing.T) {
	addr := "nonexist:///non.existent"
	cc, err := Dial(addr, WithInsecure())
	if err != nil {
		t.Fatalf("Dial(%s, _) = _, %v, want _, <nil>", addr, err)
	}
	defer cc.Close()
	if cc.Target() != addr {
		t.Fatalf("Target() = %s, want %s", cc.Target(), addr)
	}
}

type backoffForever struct{}

func (b backoffForever) Backoff(int) time.Duration { return time.Duration(math.MaxInt64) }

func (s) TestResetConnectBackoff(t *testing.T) {
	dials := make(chan struct{})
	defer func() { // If we fail, let the http2client break out of dialing.
		select {
		case <-dials:
		default:
		}
	}()
	dialer := func(string, time.Duration) (net.Conn, error) {
		dials <- struct{}{}
		return nil, errors.New("failed to fake dial")
	}
	cc, err := Dial("any", WithInsecure(), WithDialer(dialer), withBackoff(backoffForever{}))
	if err != nil {
		t.Fatalf("Dial() = _, %v; want _, nil", err)
	}
	defer cc.Close()
	select {
	case <-dials:
	case <-time.NewTimer(10 * time.Second).C:
		t.Fatal("Failed to call dial within 10s")
	}

	select {
	case <-dials:
		t.Fatal("Dial called unexpectedly before resetting backoff")
	case <-time.NewTimer(100 * time.Millisecond).C:
	}

	cc.ResetConnectBackoff()

	select {
	case <-dials:
	case <-time.NewTimer(10 * time.Second).C:
		t.Fatal("Failed to call dial within 10s after resetting backoff")
	}
}

func (s) TestBackoffCancel(t *testing.T) {
	dialStrCh := make(chan string)
	cc, err := Dial("any", WithInsecure(), WithDialer(func(t string, _ time.Duration) (net.Conn, error) {
		dialStrCh <- t
		return nil, fmt.Errorf("test dialer, always error")
	}))
	if err != nil {
		t.Fatalf("Failed to create ClientConn: %v", err)
	}
	<-dialStrCh
	cc.Close()
	// Should not leak. May need -count 5000 to exercise.
}

// UpdateAddresses should cause the next reconnect to begin from the top of the
// list if the connection is not READY.
func (s) TestUpdateAddresses_RetryFromFirstAddr(t *testing.T) {
	cleanup := setMinConnectTimeout(time.Hour)
	defer cleanup()

	lis1, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Error while listening. Err: %v", err)
	}
	defer lis1.Close()

	lis2, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Error while listening. Err: %v", err)
	}
	defer lis2.Close()

	lis3, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Error while listening. Err: %v", err)
	}
	defer lis3.Close()

	closeServer2 := make(chan struct{})
	server1ContactedFirstTime := make(chan struct{})
	server1ContactedSecondTime := make(chan struct{})
	server2ContactedFirstTime := make(chan struct{})
	server2ContactedSecondTime := make(chan struct{})
	server3Contacted := make(chan struct{})

	stateNotifications := make(chan connectivity.State, 100)
	testBalancer.ResetNotifier(stateNotifications)

	// Launch server 1.
	go func() {
		// First, let's allow the initial connection to go READY. We need to do
		// this because tryUpdateAddrs only works after there's some non-nil
		// address on the ac, and curAddress is only set after READY.
		conn1, err := lis1.Accept()
		if err != nil {
			t.Error(err)
			return
		}
		go keepReading(conn1)

		framer := http2.NewFramer(conn1, conn1)
		if err := framer.WriteSettings(http2.Setting{}); err != nil {
			t.Errorf("Error while writing settings frame. %v", err)
			return
		}

		// Wait for the transport to become ready.
		for s := range stateNotifications {
			if s == connectivity.Ready {
				break
			}
		}

		// Once it's ready, curAddress has been set. So let's close this
		// connection prompting the first reconnect cycle.
		conn1.Close()

		// Accept and immediately close, causing it to go to server2.
		conn2, err := lis1.Accept()
		if err != nil {
			t.Error(err)
			return
		}
		close(server1ContactedFirstTime)
		conn2.Close()

		// Hopefully it picks this server after tryUpdateAddrs.
		lis1.Accept()
		close(server1ContactedSecondTime)
	}()
	// Launch server 2.
	go func() {
		// Accept and then hang waiting for the test call tryUpdateAddrs and
		// then signal to this server to close. After this server closes, it
		// should start from the top instead of trying server2 or continuing
		// to server3.
		conn, err := lis2.Accept()
		if err != nil {
			t.Error(err)
			return
		}

		close(server2ContactedFirstTime)
		<-closeServer2
		conn.Close()

		// After tryUpdateAddrs, it should NOT try server2.
		lis2.Accept()
		close(server2ContactedSecondTime)
	}()
	// Launch server 3.
	go func() {
		// After tryUpdateAddrs, it should NOT try server3. (or any other time)
		lis3.Accept()
		close(server3Contacted)
	}()

	addrsList := []resolver.Address{
		{Addr: lis1.Addr().String()},
		{Addr: lis2.Addr().String()},
		{Addr: lis3.Addr().String()},
	}
	rb := manual.NewBuilderWithScheme("whatever")
	rb.InitialAddrs(addrsList)

	client, err := Dial("this-gets-overwritten", WithInsecure(), WithWaitForHandshake(), withResolverBuilder(rb), withBackoff(noBackoff{}), WithBalancerName(stateRecordingBalancerName))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	timeout := time.After(5 * time.Second)

	// Wait for server1 to be contacted (which will immediately fail), then
	// server2 (which will hang waiting for our signal).
	select {
	case <-server1ContactedFirstTime:
	case <-timeout:
		t.Fatal("timed out waiting for server1 to be contacted")
	}
	select {
	case <-server2ContactedFirstTime:
	case <-timeout:
		t.Fatal("timed out waiting for server2 to be contacted")
	}

	// Grab the addrConn and call tryUpdateAddrs.
	var ac *addrConn
	client.mu.Lock()
	for clientAC := range client.conns {
		ac = clientAC
		break
	}
	client.mu.Unlock()

	ac.acbw.UpdateAddresses(addrsList)

	// We've called tryUpdateAddrs - now let's make server2 close the
	// connection and check that it goes back to server1 instead of continuing
	// to server3 or trying server2 again.
	close(closeServer2)

	select {
	case <-server1ContactedSecondTime:
	case <-server2ContactedSecondTime:
		t.Fatal("server2 was contacted a second time, but it after tryUpdateAddrs it should have re-started the list and tried server1")
	case <-server3Contacted:
		t.Fatal("server3 was contacted, but after tryUpdateAddrs it should have re-started the list and tried server1")
	case <-timeout:
		t.Fatal("timed out waiting for any server to be contacted after tryUpdateAddrs")
	}
}

// Set the minConnectTimeout. Be sure to defer cleanup!
func setMinConnectTimeout(newMin time.Duration) (cleanup func()) {
	mctBkp := getMinConnectTimeout()
	atomic.StoreInt64((*int64)(&mutableMinConnectTimeout), int64(newMin))
	return func() {
		atomic.StoreInt64((*int64)(&mutableMinConnectTimeout), int64(mctBkp))
	}
}
