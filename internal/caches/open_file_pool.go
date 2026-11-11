// Copyright 2022 GoEdge goedge.cdn@gmail.com. All rights reserved.

package caches

import (
	"sync"

	"github.com/TeaOSLab/EdgeNode/internal/utils/fasttime"
	"github.com/TeaOSLab/EdgeNode/internal/utils/linkedlist"
)

type OpenFilePool struct {
	c        chan *OpenFile
	linkItem *linkedlist.Item[*OpenFilePool]
	filename string
	version  int64
	isClosed bool
	usedSize int64
	locker   sync.Mutex
}

func NewOpenFilePool(filename string) *OpenFilePool {
	var pool = &OpenFilePool{
		filename: filename,
		c:        make(chan *OpenFile, 1024),
		version:  fasttime.Now().UnixMilli(),
	}
	pool.linkItem = linkedlist.NewItem[*OpenFilePool](pool)
	return pool
}

func (this *OpenFilePool) Filename() string {
	return this.filename
}

func (this *OpenFilePool) Get() (resultFile *OpenFile, consumed bool, consumedSize int64) {
	this.locker.Lock()
	// 如果已经关闭，直接返回
	if this.isClosed {
		this.locker.Unlock()
		return nil, false, 0
	}

	select {
	case file := <-this.c:
		if file != nil {
			this.usedSize -= file.size
			version := this.version
			this.locker.Unlock()

			// A popped file belongs exclusively to its reader. Seek outside the
			// pool lock so readers of the same cached file can proceed together.
			err := file.SeekStart()
			if err != nil {
				_ = file.Close()
				return nil, true, file.size
			}
			file.version = version

			return file, true, file.size
		}
		this.locker.Unlock()
		return nil, false, 0
	default:
		this.locker.Unlock()
		return nil, false, 0
	}
}

func (this *OpenFilePool) Put(file *OpenFile) bool {
	this.locker.Lock()
	// 如果已关闭，则不接受新的文件
	if this.isClosed {
		this.locker.Unlock()
		_ = file.Close()
		return false
	}

	// 检查文件版本号
	if this.version > 0 && file.version > 0 && file.version != this.version {
		this.locker.Unlock()
		_ = file.Close()
		return false
	}

	// 加入Pool
	select {
	case this.c <- file:
		this.usedSize += file.size
		this.locker.Unlock()
		return true
	default:
		this.locker.Unlock()
		// 多余的直接关闭
		_ = file.Close()
		return false
	}
}

func (this *OpenFilePool) Len() int {
	return len(this.c)
}

func (this *OpenFilePool) TotalSize() int64 {
	this.locker.Lock()
	defer this.locker.Unlock()
	return this.usedSize
}

func (this *OpenFilePool) SetClosing() {
	this.locker.Lock()
	defer this.locker.Unlock()
	this.isClosed = true
}

func (this *OpenFilePool) Close() {
	this.locker.Lock()
	this.isClosed = true
	var files []*OpenFile
	for {
		select {
		case file := <-this.c:
			files = append(files, file)
		default:
			this.usedSize = 0
			this.locker.Unlock()
			for _, file := range files {
				_ = file.Close()
			}
			return
		}
	}
}
