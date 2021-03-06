// Copyright (C) 2015,2016 Nippon Telegraph and Telephone Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/armon/go-radix"
	"github.com/osrg/gobgp/config"
	"github.com/osrg/gobgp/packet/bgp"
	"github.com/osrg/gobgp/packet/rtr"
	"github.com/osrg/gobgp/table"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

const (
	CONNECT_RETRY_INTERVAL = 30
)

func before(a, b uint32) bool {
	return int32(a-b) < 0
}

type RoaBucket struct {
	Prefix  *table.IPPrefix
	entries []*table.ROA
}

func (r *RoaBucket) GetEntries() []*table.ROA {
	return r.entries
}

type roas []*table.ROA

func (r roas) Len() int {
	return len(r)
}

func (r roas) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}

func (r roas) Less(i, j int) bool {
	r1 := r[i]
	r2 := r[j]

	if r1.MaxLen < r1.MaxLen {
		return true
	} else if r1.MaxLen > r1.MaxLen {
		return false
	}

	if r1.AS < r2.AS {
		return true
	}
	return false
}

type ROAEventType uint8

const (
	CONNECTED ROAEventType = iota
	DISCONNECTED
	RTR
	LIFETIMEOUT
)

type ROAEvent struct {
	EventType ROAEventType
	Src       string
	Data      []byte
	conn      *net.TCPConn
}

type roaManager struct {
	AS        uint32
	Roas      map[bgp.RouteFamily]*radix.Tree
	eventCh   chan *ROAEvent
	clientMap map[string]*roaClient
}

func NewROAManager(as uint32) (*roaManager, error) {
	m := &roaManager{
		AS:   as,
		Roas: make(map[bgp.RouteFamily]*radix.Tree),
	}
	m.Roas[bgp.RF_IPv4_UC] = radix.New()
	m.Roas[bgp.RF_IPv6_UC] = radix.New()
	m.eventCh = make(chan *ROAEvent)
	m.clientMap = make(map[string]*roaClient)
	return m, nil
}

func (m *roaManager) SetAS(as uint32) error {
	if m.AS != 0 {
		return fmt.Errorf("AS was already configured")
	}
	m.AS = as
	return nil
}

func (m *roaManager) AddServer(host string, lifetime int64) error {
	if m.AS == 0 {
		return fmt.Errorf("AS isn't configured yet")
	}
	address, port, err := net.SplitHostPort(host)
	if err != nil {
		return err
	}
	if lifetime == 0 {
		lifetime = 3600
	}
	if _, ok := m.clientMap[host]; ok {
		return fmt.Errorf("ROA server exists %s", host)
	}
	m.clientMap[host] = NewRoaClient(address, port, m.eventCh, lifetime)
	return nil
}

func (m *roaManager) DeleteServer(host string) error {
	client, ok := m.clientMap[host]
	if !ok {
		return fmt.Errorf("ROA server doesn't exists %s", host)
	}
	client.stop()
	m.deleteAllROA(host)
	delete(m.clientMap, host)
	return nil
}

func (m *roaManager) deleteAllROA(network string) {
	for _, tree := range m.Roas {
		deleteKeys := make([]string, 0, tree.Len())
		tree.Walk(func(s string, v interface{}) bool {
			b, _ := v.(*RoaBucket)
			newEntries := make([]*table.ROA, 0, len(b.entries))
			for _, r := range b.entries {
				if r.Src != network {
					newEntries = append(newEntries, r)
				}
			}
			if len(newEntries) > 0 {
				b.entries = newEntries
			} else {
				deleteKeys = append(deleteKeys, s)
			}
			return false
		})
		for _, key := range deleteKeys {
			tree.Delete(key)
		}
	}
}

func (m *roaManager) Enable(address string) error {
	for network, client := range m.clientMap {
		add, _, _ := net.SplitHostPort(network)
		if add == address {
			client.enable(client.serialNumber)
			return nil
		}
	}
	return fmt.Errorf("ROA server not found %s", address)
}

func (m *roaManager) Disable(address string) error {
	for network, client := range m.clientMap {
		add, _, _ := net.SplitHostPort(network)
		if add == address {
			client.reset()
			m.deleteAllROA(add)
			return nil
		}
	}
	return fmt.Errorf("ROA server not found %s", address)
}

func (m *roaManager) Reset(address string) error {
	return m.Disable(address)
}

func (m *roaManager) SoftReset(address string) error {
	for network, client := range m.clientMap {
		add, _, _ := net.SplitHostPort(network)
		if add == address {
			client.softReset()
			m.deleteAllROA(network)
			return nil
		}
	}
	return fmt.Errorf("ROA server not found %s", address)
}

func (c *roaManager) ReceiveROA() chan *ROAEvent {
	return c.eventCh
}

func (c *roaClient) lifetimeout() {
	c.eventCh <- &ROAEvent{
		EventType: LIFETIMEOUT,
		Src:       c.host,
	}
}

func (m *roaManager) HandleROAEvent(ev *ROAEvent) {
	client, y := m.clientMap[ev.Src]
	if !y {
		if ev.EventType == CONNECTED {
			ev.conn.Close()
		}
		log.WithFields(log.Fields{"Topic": "rpki"}).Errorf("Can't find %s ROA server configuration", ev.Src)
		return
	}
	switch ev.EventType {
	case DISCONNECTED:
		log.WithFields(log.Fields{"Topic": "rpki"}).Infof("ROA server %s is disconnected", ev.Src)
		client.state.Downtime = time.Now().Unix()
		// clear state
		client.endOfData = false
		client.pendingROAs = make([]*table.ROA, 0)
		client.state.RpkiMessages = config.RpkiMessages{}
		client.conn = nil
		go client.tryConnect()
		client.timer = time.AfterFunc(time.Duration(client.lifetime)*time.Second, client.lifetimeout)
		client.oldSessionID = client.sessionID
	case CONNECTED:
		log.WithFields(log.Fields{"Topic": "rpki"}).Infof("ROA server %s is connected", ev.Src)
		client.conn = ev.conn
		client.state.Uptime = time.Now().Unix()
		go client.established()
	case RTR:
		m.handleRTRMsg(client, &client.state, ev.Data)
	case LIFETIMEOUT:
		// a) already reconnected but hasn't received
		// EndOfData -> needs to delete stale ROAs
		// b) not reconnected -> needs to delete stale ROAs
		//
		// c) already reconnected and received EndOfData so
		// all stale ROAs were deleted -> timer was cancelled
		// so should not be here.
		if client.oldSessionID != client.sessionID {
			log.WithFields(log.Fields{"Topic": "rpki"}).Infof("Reconnected to %s. Ignore timeout", client.host)
		} else {
			log.WithFields(log.Fields{"Topic": "rpki"}).Infof("Deleting all ROAs due to timeout with:%s", client.host)
			m.deleteAllROA(client.host)
		}
	}
}

func (m *roaManager) roa2tree(roa *table.ROA) (*radix.Tree, string) {
	tree := m.Roas[bgp.RF_IPv4_UC]
	if roa.Family == bgp.AFI_IP6 {
		tree = m.Roas[bgp.RF_IPv6_UC]
	}
	return tree, table.IpToRadixkey(roa.Prefix.Prefix, roa.Prefix.Length)
}

func (m *roaManager) deleteROA(roa *table.ROA) {
	tree, key := m.roa2tree(roa)
	b, _ := tree.Get(key)
	if b != nil {
		bucket := b.(*RoaBucket)
		newEntries := make([]*table.ROA, 0, len(bucket.entries))
		for _, r := range bucket.entries {
			if !r.Equal(roa) {
				newEntries = append(newEntries, r)
			}
		}
		if len(newEntries) != len(bucket.entries) {
			bucket.entries = newEntries
			if len(newEntries) == 0 {
				tree.Delete(key)
			}
			return
		}
	}
	log.WithFields(log.Fields{
		"Topic":         "rpki",
		"Prefix":        roa.Prefix.Prefix.String(),
		"Prefix Length": roa.Prefix.Length,
		"AS":            roa.AS,
		"Max Length":    roa.MaxLen,
	}).Info("Can't withdraw a ROA")
}

func (m *roaManager) addROA(roa *table.ROA) {
	tree, key := m.roa2tree(roa)
	b, _ := tree.Get(key)
	var bucket *RoaBucket
	if b == nil {
		bucket = &RoaBucket{
			Prefix:  roa.Prefix,
			entries: make([]*table.ROA, 0),
		}
		tree.Insert(key, bucket)
	} else {
		bucket = b.(*RoaBucket)
		for _, r := range bucket.entries {
			if r.Equal(roa) {
				// we already have the same one
				return
			}
		}
	}
	bucket.entries = append(bucket.entries, roa)
}

func (c *roaManager) handleRTRMsg(client *roaClient, state *config.RpkiServerState, buf []byte) {
	received := &state.RpkiMessages.RpkiReceived

	m, err := rtr.ParseRTR(buf)
	if err == nil {
		switch msg := m.(type) {
		case *rtr.RTRSerialNotify:
			if before(client.serialNumber, msg.RTRCommon.SerialNumber) {
				client.enable(client.serialNumber)
			} else if client.serialNumber == msg.RTRCommon.SerialNumber {
				// nothing
			} else {
				// should not happen. try to get the whole ROAs.
				client.softReset()
			}
			received.SerialNotify++
		case *rtr.RTRSerialQuery:
		case *rtr.RTRResetQuery:
		case *rtr.RTRCacheResponse:
			received.CacheResponse++
			client.endOfData = false
		case *rtr.RTRIPPrefix:
			family := bgp.AFI_IP
			if msg.Type == rtr.RTR_IPV4_PREFIX {
				received.Ipv4Prefix++
			} else {
				family = bgp.AFI_IP6
				received.Ipv6Prefix++
			}
			roa := table.NewROA(family, msg.Prefix, msg.PrefixLen, msg.MaxLen, msg.AS, client.host)
			if (msg.Flags & 1) == 1 {
				if client.endOfData {
					c.addROA(roa)
				} else {
					client.pendingROAs = append(client.pendingROAs, roa)
				}
			} else {
				c.deleteROA(roa)
			}
		case *rtr.RTREndOfData:
			received.EndOfData++
			if client.sessionID != msg.RTRCommon.SessionID {
				// remove all ROAs related with the
				// previous session
				c.deleteAllROA(client.host)
			}
			client.sessionID = msg.RTRCommon.SessionID
			client.serialNumber = msg.RTRCommon.SerialNumber
			client.endOfData = true
			if client.timer != nil {
				client.timer.Stop()
				client.timer = nil
			}
			for _, roa := range client.pendingROAs {
				c.addROA(roa)
			}
			client.pendingROAs = make([]*table.ROA, 0)
		case *rtr.RTRCacheReset:
			client.softReset()
			received.CacheReset++
		case *rtr.RTRErrorReport:
			received.Error++
		}
	} else {
		log.WithFields(log.Fields{
			"Topic": "rpki",
			"Host":  client.host,
			"Error": err,
		}).Info("Failed to parse an RTR message")
	}
}

func (c *roaManager) GetServers() []*config.RpkiServer {
	f := func(tree *radix.Tree) (map[string]uint32, map[string]uint32) {
		records := make(map[string]uint32)
		prefixes := make(map[string]uint32)

		tree.Walk(func(s string, v interface{}) bool {
			b, _ := v.(*RoaBucket)
			tmpRecords := make(map[string]uint32)
			for _, roa := range b.entries {
				tmpRecords[roa.Src]++
			}

			for src, r := range tmpRecords {
				if r > 0 {
					records[src] += r
					prefixes[src]++
				}
			}
			return false
		})
		return records, prefixes
	}

	recordsV4, prefixesV4 := f(c.Roas[bgp.RF_IPv4_UC])
	recordsV6, prefixesV6 := f(c.Roas[bgp.RF_IPv6_UC])

	l := make([]*config.RpkiServer, 0, len(c.clientMap))
	for _, client := range c.clientMap {
		state := &client.state

		if client.conn == nil {
			state.Up = false
		} else {
			state.Up = true
		}
		f := func(m map[string]uint32, key string) uint32 {
			if r, ok := m[key]; ok {
				return r
			}
			return 0
		}
		state.RecordsV4 = f(recordsV4, client.host)
		state.RecordsV6 = f(recordsV6, client.host)
		state.PrefixesV4 = f(prefixesV4, client.host)
		state.PrefixesV6 = f(prefixesV6, client.host)
		state.SerialNumber = client.serialNumber

		addr, port, _ := net.SplitHostPort(client.host)
		l = append(l, &config.RpkiServer{
			Config: config.RpkiServerConfig{
				Address: addr,
				Port:    func() uint32 { p, _ := strconv.Atoi(port); return uint32(p) }(),
			},
			State: client.state,
		})
	}
	return l
}

func (c *roaManager) GetRoa(family bgp.RouteFamily) ([]*table.ROA, error) {
	if len(c.clientMap) == 0 {
		return []*table.ROA{}, fmt.Errorf("RPKI server isn't configured.")
	}
	var rfList []bgp.RouteFamily
	switch family {
	case bgp.RF_IPv4_UC:
		rfList = []bgp.RouteFamily{bgp.RF_IPv4_UC}
	case bgp.RF_IPv6_UC:
		rfList = []bgp.RouteFamily{bgp.RF_IPv6_UC}
	default:
		rfList = []bgp.RouteFamily{bgp.RF_IPv4_UC, bgp.RF_IPv6_UC}
	}
	l := make([]*table.ROA, 0)
	for _, rf := range rfList {
		if tree, ok := c.Roas[rf]; ok {
			tree.Walk(func(s string, v interface{}) bool {
				b, _ := v.(*RoaBucket)
				var roaList roas
				for _, r := range b.entries {
					roaList = append(roaList, r)
				}
				sort.Sort(roaList)
				for _, roa := range roaList {
					l = append(l, roa)
				}
				return false
			})
		}
	}
	return l, nil
}

func ValidatePath(ownAs uint32, tree *radix.Tree, cidr string, asPath *bgp.PathAttributeAsPath) (config.RpkiValidationResultType, *RoaBucket) {
	var as uint32

	if asPath == nil || len(asPath.Value) == 0 {
		as = ownAs
	} else {
		asParam := asPath.Value[len(asPath.Value)-1].(*bgp.As4PathParam)
		switch asParam.Type {
		case bgp.BGP_ASPATH_ATTR_TYPE_SEQ:
			if len(asParam.AS) == 0 {
				as = ownAs
			} else {
				as = asParam.AS[len(asParam.AS)-1]
			}
		case bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SET, bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SEQ:
			as = ownAs
		default:
			return config.RPKI_VALIDATION_RESULT_TYPE_NOT_FOUND, nil
		}
	}
	_, n, _ := net.ParseCIDR(cidr)
	ones, _ := n.Mask.Size()
	prefixLen := uint8(ones)
	key := table.IpToRadixkey(n.IP, prefixLen)
	_, b, _ := tree.LongestPrefix(key)
	if b == nil {
		return config.RPKI_VALIDATION_RESULT_TYPE_NOT_FOUND, nil
	}

	result := config.RPKI_VALIDATION_RESULT_TYPE_INVALID
	var bucket *RoaBucket
	fn := radix.WalkFn(func(k string, v interface{}) bool {
		bucket, _ = v.(*RoaBucket)
		for _, r := range bucket.entries {
			if prefixLen <= r.MaxLen && r.AS != 0 && r.AS == as {
				result = config.RPKI_VALIDATION_RESULT_TYPE_VALID
				return true
			}
		}
		return false
	})
	tree.WalkPath(key, fn)
	return result, bucket
}

func (c *roaManager) validate(pathList []*table.Path) {
	if len(c.clientMap) == 0 {
		// RPKI isn't enabled
		return
	}

	for _, path := range pathList {
		if path.IsWithdraw || path.IsEOR() {
			continue
		}
		if tree, ok := c.Roas[path.GetRouteFamily()]; ok {
			r, _ := ValidatePath(c.AS, tree, path.GetNlri().String(), path.GetAsPath())
			path.SetValidation(config.RpkiValidationResultType(r))
		}
	}
}

type roaClient struct {
	host         string
	conn         *net.TCPConn
	state        config.RpkiServerState
	eventCh      chan *ROAEvent
	sessionID    uint16
	oldSessionID uint16
	serialNumber uint32
	timer        *time.Timer
	lifetime     int64
	endOfData    bool
	pendingROAs  []*table.ROA
	cancelfnc    context.CancelFunc
	ctx          context.Context
}

func NewRoaClient(address, port string, ch chan *ROAEvent, lifetime int64) *roaClient {
	ctx, cancel := context.WithCancel(context.Background())
	c := &roaClient{
		host:        net.JoinHostPort(address, port),
		eventCh:     ch,
		lifetime:    lifetime,
		pendingROAs: make([]*table.ROA, 0),
		ctx:         ctx,
		cancelfnc:   cancel,
	}
	go c.tryConnect()
	return c
}

func (c *roaClient) enable(serial uint32) error {
	if c.conn != nil {
		r := rtr.NewRTRSerialQuery(c.sessionID, serial)
		data, _ := r.Serialize()
		_, err := c.conn.Write(data)
		if err != nil {
			return err
		}
		c.state.RpkiMessages.RpkiSent.SerialQuery++
	}
	return nil
}

func (c *roaClient) softReset() error {
	if c.conn != nil {
		r := rtr.NewRTRResetQuery()
		data, _ := r.Serialize()
		_, err := c.conn.Write(data)
		if err != nil {
			return err
		}
		c.state.RpkiMessages.RpkiSent.ResetQuery++
		c.endOfData = false
		c.pendingROAs = make([]*table.ROA, 0)
	}
	return nil
}

func (c *roaClient) reset() {
	if c.conn != nil {
		c.conn.Close()
	}
}

func (c *roaClient) stop() {
	c.cancelfnc()
	c.reset()
}

func (c *roaClient) tryConnect() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		if conn, err := net.Dial("tcp", c.host); err != nil {
			// better to use context with timeout
			time.Sleep(CONNECT_RETRY_INTERVAL * time.Second)
		} else {
			c.eventCh <- &ROAEvent{
				EventType: CONNECTED,
				Src:       c.host,
				conn:      conn.(*net.TCPConn),
			}
			return
		}
	}
}

func (c *roaClient) established() (err error) {
	defer func() {
		c.conn.Close()
		c.eventCh <- &ROAEvent{
			EventType: DISCONNECTED,
			Src:       c.host,
		}
	}()

	if err := c.softReset(); err != nil {
		return err
	}

	for {
		header := make([]byte, rtr.RTR_MIN_LEN)
		if _, err = io.ReadFull(c.conn, header); err != nil {
			return err
		}
		totalLen := binary.BigEndian.Uint32(header[4:8])
		if totalLen < rtr.RTR_MIN_LEN {
			return fmt.Errorf("too short header length %v", totalLen)
		}

		body := make([]byte, totalLen-rtr.RTR_MIN_LEN)
		if _, err = io.ReadFull(c.conn, body); err != nil {
			return
		}

		c.eventCh <- &ROAEvent{
			EventType: RTR,
			Src:       c.host,
			Data:      append(header, body...),
		}
	}
}
