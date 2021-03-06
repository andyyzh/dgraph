/*
 * Copyright 2016-2018 Dgraph Labs, Inc. and Contributors
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
 */

package conn

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dgraph-io/dgo/protos/api"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/x"
	"github.com/golang/glog"
	"go.opencensus.io/plugin/ocgrpc"

	"google.golang.org/grpc"
)

var (
	ErrNoConnection        = fmt.Errorf("No connection exists")
	ErrUnhealthyConnection = fmt.Errorf("Unhealthy connection")
	echoDuration           = 500 * time.Millisecond
)

// "Pool" is used to manage the grpc client connection(s) for communicating with other
// worker instances.  Right now it just holds one of them.
type Pool struct {
	sync.RWMutex
	// A "pool" now consists of one connection.  gRPC uses HTTP2 transport to combine
	// messages in the same TCP stream.
	conn *grpc.ClientConn

	lastEcho time.Time
	Addr     string
	ticker   *time.Ticker
}

type Pools struct {
	sync.RWMutex
	all map[string]*Pool
}

var pi *Pools

func init() {
	pi = new(Pools)
	pi.all = make(map[string]*Pool)
}

func Get() *Pools {
	return pi
}

func (p *Pools) Get(addr string) (*Pool, error) {
	p.RLock()
	defer p.RUnlock()
	pool, ok := p.all[addr]
	if !ok {
		return nil, ErrNoConnection
	}
	if !pool.IsHealthy() {
		return nil, ErrUnhealthyConnection
	}
	return pool, nil
}

func (p *Pools) RemoveInvalid(state *pb.MembershipState) {
	// Keeps track of valid IP addresses, assigned to active nodes. We do this
	// to avoid removing valid IP addresses from the Removed list.
	validAddr := make(map[string]struct{})
	for _, group := range state.Groups {
		for _, member := range group.Members {
			validAddr[member.Addr] = struct{}{}
		}
	}
	for _, member := range state.Zeros {
		validAddr[member.Addr] = struct{}{}
	}
	for _, member := range state.Removed {
		// Some nodes could have the same IP address. So, check before disconnecting.
		if _, valid := validAddr[member.Addr]; !valid {
			p.remove(member.Addr)
		}
	}
}

func (p *Pools) remove(addr string) {
	p.Lock()
	pool, ok := p.all[addr]
	if !ok {
		p.Unlock()
		return
	}
	glog.Warningf("DISCONNECTING from %s\n", addr)
	delete(p.all, addr)
	p.Unlock()
	pool.shutdown()
}

func (p *Pools) Connect(addr string) *Pool {
	p.RLock()
	existingPool, has := p.all[addr]
	if has {
		p.RUnlock()
		return existingPool
	}
	p.RUnlock()

	pool, err := NewPool(addr)
	if err != nil {
		glog.Errorf("Unable to connect to host: %s", addr)
		return nil
	}

	p.Lock()
	existingPool, has = p.all[addr]
	if has {
		p.Unlock()
		return existingPool
	}
	glog.Infof("CONNECTED to %v\n", addr)
	p.all[addr] = pool
	p.Unlock()
	return pool
}

// NewPool creates a new "pool" with one gRPC connection, refcount 0.
func NewPool(addr string) (*Pool, error) {
	conn, err := grpc.Dial(addr,
		grpc.WithStatsHandler(&ocgrpc.ClientHandler{}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(x.GrpcMaxSize),
			grpc.MaxCallSendMsgSize(x.GrpcMaxSize)),
		grpc.WithBackoffMaxDelay(time.Second),
		grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	pl := &Pool{conn: conn, Addr: addr, lastEcho: time.Now()}

	// Initialize ticker before running monitor health.
	pl.ticker = time.NewTicker(echoDuration)
	go pl.MonitorHealth()
	return pl, nil
}

// Get returns the connection to use from the pool of connections.
func (p *Pool) Get() *grpc.ClientConn {
	p.RLock()
	defer p.RUnlock()
	return p.conn
}

func (p *Pool) shutdown() {
	p.ticker.Stop()
	p.conn.Close()
}

func (p *Pool) SetUnhealthy() {
	p.Lock()
	p.lastEcho = time.Time{}
	p.Unlock()
}

func (p *Pool) listenToHeartbeat() error {
	conn := p.Get()
	c := pb.NewRaftClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.Heartbeat(ctx, &api.Payload{})
	if err != nil {
		return err
	}

	for {
		_, err := stream.Recv()
		if err != nil {
			return err
		}
		// We do this periodic stream receive based approach to defend against network partitions.
		p.Lock()
		p.lastEcho = time.Now()
		p.Unlock()
	}
}

// MonitorHealth monitors the health of the connection via Echo. This function blocks forever.
func (p *Pool) MonitorHealth() {
	var lastErr error
	for range p.ticker.C {
		err := p.listenToHeartbeat()
		if lastErr != nil && err == nil {
			glog.Infof("Connection established with %v\n", p.Addr)
		} else if err != nil && lastErr == nil {
			glog.Warningf("Connection lost with %v. Error: %v\n", p.Addr, err)
		}
		lastErr = err
		// Sleep for a bit before retrying.
		time.Sleep(echoDuration)
	}
}

func (p *Pool) IsHealthy() bool {
	if p == nil {
		return false
	}
	p.RLock()
	defer p.RUnlock()
	return time.Since(p.lastEcho) < 4*echoDuration
}
