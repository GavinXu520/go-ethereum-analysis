// Copyright 2016 The github.com/go-ethereum-analysis Authors
// This file is part of the github.com/go-ethereum-analysis library.
//
// The github.com/go-ethereum-analysis library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The github.com/go-ethereum-analysis library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the github.com/go-ethereum-analysis library. If not, see <http://www.gnu.org/licenses/>.

// Package flowcontrol implements a client side flow control mechanism
package flowcontrol

import (
	"sync"
	"time"

	"github.com/go-ethereum-analysis/common/mclock"
)

const fcTimeConst = time.Millisecond

// 握手时的重要参数
type ServerParams struct {
	BufLimit, MinRecharge uint64
}


// 这是实现一个 轻节点链接的 client端 (就是一个轻节点)
type ClientNode struct {
	params   *ServerParams
	bufValue uint64
	lastTime mclock.AbsTime
	lock     sync.Mutex
	cm       *ClientManager
	cmNode   *cmNode
}

func NewClientNode(cm *ClientManager, params *ServerParams) *ClientNode {
	node := &ClientNode{
		cm:       cm,
		params:   params,
		bufValue: params.BufLimit,
		lastTime: mclock.Now(),
	}
	node.cmNode = cm.addNode(node)
	return node
}

func (peer *ClientNode) Remove(cm *ClientManager) {
	cm.removeNode(peer.cmNode)
}

func (peer *ClientNode) recalcBV(time mclock.AbsTime) {
	dt := uint64(time - peer.lastTime)
	if time < peer.lastTime {
		dt = 0
	}
	peer.bufValue += peer.params.MinRecharge * dt / uint64(fcTimeConst)
	if peer.bufValue > peer.params.BufLimit {
		peer.bufValue = peer.params.BufLimit
	}
	peer.lastTime = time
}

func (peer *ClientNode) AcceptRequest() (uint64, bool) {
	peer.lock.Lock()
	defer peer.lock.Unlock()

	time := mclock.Now()
	peer.recalcBV(time)
	return peer.bufValue, peer.cm.accept(peer.cmNode, time)
}

func (peer *ClientNode) RequestProcessed(cost uint64) (bv, realCost uint64) {
	peer.lock.Lock()
	defer peer.lock.Unlock()

	time := mclock.Now()
	peer.recalcBV(time)
	peer.bufValue -= cost
	peer.recalcBV(time)
	rcValue, rcost := peer.cm.processed(peer.cmNode, time)
	if rcValue < peer.params.BufLimit {
		bv := peer.params.BufLimit - rcValue
		if bv > peer.bufValue {
			peer.bufValue = bv
		}
	}
	return peer.bufValue, rcost
}

// 这是实现一个轻节点链接的 Server端 (一个全节点)
type ServerNode struct {
	bufEstimate uint64
	lastTime    mclock.AbsTime
	params      *ServerParams
	sumCost     uint64            // sum of req costs sent to this server
	pending     map[uint64]uint64 // value = sumCost after sending the given req
	lock        sync.RWMutex
}

func NewServerNode(params *ServerParams) *ServerNode {
	return &ServerNode{
		bufEstimate: params.BufLimit,
		lastTime:    mclock.Now(),
		params:      params,
		pending:     make(map[uint64]uint64),
	}
}

func (peer *ServerNode) recalcBLE(time mclock.AbsTime) {
	dt := uint64(time - peer.lastTime)
	if time < peer.lastTime {
		dt = 0
	}
	peer.bufEstimate += peer.params.MinRecharge * dt / uint64(fcTimeConst)
	if peer.bufEstimate > peer.params.BufLimit {
		peer.bufEstimate = peer.params.BufLimit
	}
	peer.lastTime = time
}

// safetyMargin is added to the flow control waiting time when estimated buffer value is low
//
// 当估计的缓冲区值较低时，将safetyMargin添加到流控制等待时间
const safetyMargin = time.Millisecond


//握手时声明的3个参数为：
//
// Buffer Limit
// Maximum Request Cost table
// Minimum Rate of Recharge
//
func (peer *ServerNode) canSend(maxCost uint64) (time.Duration, float64) {
	peer.recalcBLE(mclock.Now())
	maxCost += uint64(safetyMargin) * peer.params.MinRecharge / uint64(fcTimeConst)
	if maxCost > peer.params.BufLimit {
		maxCost = peer.params.BufLimit
	}
	if peer.bufEstimate >= maxCost {
		return 0, float64(peer.bufEstimate-maxCost) / float64(peer.params.BufLimit)
	}
	return time.Duration((maxCost - peer.bufEstimate) * uint64(fcTimeConst) / peer.params.MinRecharge), 0
}

// CanSend returns the minimum waiting time required before sending a request
// with the given maximum estimated cost. Second return value is the relative
// estimated buffer level after sending the request (divided by BufLimit).
//
//
// CanSend
// 返回以给定的最大估计成本发送请求之前所需的最短等待时间。
// 第二个返回值是发送请求后的相对估计缓冲区级别（由BufLimit划分）。
func (peer *ServerNode) CanSend(maxCost uint64) (time.Duration, float64) {
	peer.lock.RLock()
	defer peer.lock.RUnlock()

	return peer.canSend(maxCost)
}

// QueueRequest should be called when the request has been assigned to the given
// server node, before putting it in the send queue. It is mandatory that requests
// are sent in the same order as the QueueRequest calls are made.
func (peer *ServerNode) QueueRequest(reqID, maxCost uint64) {
	peer.lock.Lock()
	defer peer.lock.Unlock()

	peer.bufEstimate -= maxCost
	peer.sumCost += maxCost
	peer.pending[reqID] = peer.sumCost
}

// GotReply adjusts estimated buffer value according to the value included in
// the latest request reply.
func (peer *ServerNode) GotReply(reqID, bv uint64) {

	peer.lock.Lock()
	defer peer.lock.Unlock()

	if bv > peer.params.BufLimit {
		bv = peer.params.BufLimit
	}
	sc, ok := peer.pending[reqID]
	if !ok {
		return
	}
	delete(peer.pending, reqID)
	cc := peer.sumCost - sc
	peer.bufEstimate = 0
	if bv > cc {
		peer.bufEstimate = bv - cc
	}
	peer.lastTime = mclock.Now()
}
