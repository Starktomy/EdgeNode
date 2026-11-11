// Copyright 2023 GoEdge CDN goedge.cdn@gmail.com. All rights reserved. Official site: https://goedge.cloud .

package nodes

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	teaconst "github.com/TeaOSLab/EdgeNode/internal/const"
	"github.com/TeaOSLab/EdgeNode/internal/utils/fasttime"
	"github.com/TeaOSLab/EdgeNode/internal/utils/goman"
	"github.com/TeaOSLab/EdgeNode/internal/utils/zero"
)

const originConnCloseDelaySeconds = 3

var closingOriginConnMap = map[*OriginConn]zero.Zero{}
var closingOriginConnLocker = &sync.RWMutex{}

func init() {
	if !teaconst.IsMain {
		return
	}

	goman.New(func() {
		var ticker = time.NewTicker(originConnCloseDelaySeconds * time.Second)
		for range ticker.C {
			CleanOriginConnsTask()
		}
	})
}

func CleanOriginConnsTask() {
	var closingConns = []*OriginConn{}

	closingOriginConnLocker.RLock()
	for conn := range closingOriginConnMap {
		if conn.IsExpired() {
			closingConns = append(closingConns, conn)
		}
	}
	closingOriginConnLocker.RUnlock()

	if len(closingConns) > 0 {
		for _, conn := range closingConns {
			_ = conn.ForceClose()
			closingOriginConnLocker.Lock()
			delete(closingOriginConnMap, conn)
			closingOriginConnLocker.Unlock()
		}
	}
}

// OriginConn connection with origin site
type OriginConn struct {
	net.Conn

	lastReadOk atomic.Bool
	lastReadAt atomic.Int64
	isClosed   atomic.Bool
}

// NewOriginConn create new origin connection
func NewOriginConn(rawConn net.Conn) net.Conn {
	return &OriginConn{Conn: rawConn}
}

// Read implement Read() for net.Conn interface
func (this *OriginConn) Read(b []byte) (n int, err error) {
	n, err = this.Conn.Read(b)
	this.lastReadOk.Store(err == nil)
	if err == nil {
		this.lastReadAt.Store(fasttime.Now().Unix())
	}
	return
}

// Close implement Close() for net.Conn interface
func (this *OriginConn) Close() error {
	if this.lastReadOk.Load() && !this.isClosed.Load() && fasttime.Now().Unix()-this.lastReadAt.Load() <= originConnCloseDelaySeconds {
		closingOriginConnLocker.Lock()
		closingOriginConnMap[this] = zero.Zero{}
		closingOriginConnLocker.Unlock()
		return nil
	}

	return this.ForceClose()
}

func (this *OriginConn) ForceClose() error {
	if this.isClosed.Swap(true) {
		return nil
	}

	return this.Conn.Close()
}

func (this *OriginConn) IsExpired() bool {
	return fasttime.Now().Unix()-this.lastReadAt.Load() > originConnCloseDelaySeconds
}
