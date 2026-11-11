// Copyright 2021 GoEdge goedge.cdn@gmail.com. All rights reserved.

package nodes

import (
	"net/http"
)

func (this *HTTPRequest) doRequestLimit() (shouldStop bool) {
	// Enforce entity size even when the peer is otherwise exempt from rate limits.
	if err := this.prepareRequestBodyLimit(); err != nil {
		this.writer.Send(http.StatusRequestEntityTooLarge, "Request body too large")
		return true
	}

	// 是否在全局名单中
	_, isInAllowedList, _ := this.allowClientIP(this.RemoteAddr())
	if isInAllowedList {
		return false
	}

	// 设置连接相关参数
	if this.web.RequestLimit.MaxConns > 0 || this.web.RequestLimit.MaxConnsPerIP > 0 {
		var requestConn = this.RawReq.Context().Value(HTTPConnContextKey)
		if requestConn != nil {
			clientConn, ok := requestConn.(ClientConnInterface)
			if ok && !clientConn.IsBound() {
				if !clientConn.Bind(this.ReqServer.Id, this.requestRemoteAddr(true), this.web.RequestLimit.MaxConns, this.web.RequestLimit.MaxConnsPerIP) {
					this.writeCode(http.StatusTooManyRequests, "", "")
					this.Close()
					return true
				}
			}
		}
	}

	return false
}
