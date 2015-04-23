package memory

import (
	"container/list"
	"fmt"
	log "github.com/blackbeans/log4go"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

//内存的快照
type MemorySnapshot struct {
	filePath     string
	baseDir      *os.File
	segments     Segments
	chunkId      int64
	writeChannel chan *Chunk
	running      bool
	waitSync     *sync.WaitGroup
	batchSize    int
	segcacheSize int        //segment cache size
	segmentCache *list.List //segment cached
	sync.RWMutex
}

func NewMemorySnapshot(filePath string, batchSize int, segcacheSize int) *MemorySnapshot {
	ms := &MemorySnapshot{
		chunkId:      -1,
		filePath:     filePath,
		segments:     make(Segments, 0, 50),
		writeChannel: make(chan *Chunk, 10000),
		running:      true,
		batchSize:    batchSize,
		segcacheSize: segcacheSize,
		segmentCache: list.New(),
		waitSync:     &sync.WaitGroup{}}
	ms.load()

	go ms.sync()
	ms.waitSync.Add(1)
	return ms
}

func (self *MemorySnapshot) load() {
	log.Info("MemorySnapshot|Load Segments ...")

	if !dirExist(self.filePath) {
		err := os.MkdirAll(self.filePath, os.ModePerm)
		if nil != err {
			log.Error("MemorySnapshot|Load Segments|MKDIR|FAIL|%s|%s", err, self.filePath)
			panic(err)
		}
	}

	bashDir, err := os.Open(self.filePath)
	if nil != err {
		log.Error("MemorySnapshot|Load Segments|FAIL|%s|%s", err, self.filePath)
		panic(err)
	}

	self.baseDir = bashDir

	//fetch all Segment
	filepath.Walk(self.filePath, func(path string, f os.FileInfo, err error) error {
		if !f.IsDir() {
			name := strings.TrimSuffix(f.Name(), SEGMENT_DATA_SUFFIX)
			split := strings.SplitN(name, "-", 2)
			sid := int64(0)
			if len(split) >= 2 {
				id, err := strconv.ParseInt(split[1], 10, 64)
				if nil != err {
					log.Error("MemorySnapshot|Load Segments|Parse SegmentId|FAIL|%s|%s", err, name)
					return err
				}
				sid = id
			}

			// create segment
			seg := &Segment{
				path: path,
				name: f.Name(),
				sid:  sid}

			self.segments = append(self.segments, seg)
			log.Info("MemorySnapshot|load|init Segment|%s", path)
		}

		return nil
	})

	//sort segments
	sort.Sort(self.segments)

	//check roll
	self.checkRoll()

	//recover snapshost
	self.recoverSnapshot()

	//load fixed num  segments into memory

	log.Info("MemorySnapshot|Load|SUCC|%s", self)
}

func (self *MemorySnapshot) recoverSnapshot() {
	//current segmentid
	if len(self.segments) > 0 {
		s := self.segments[len(self.segments)-1]
		err := s.Open()
		if nil != err {
			panic("MemorySnapshot|Load Last Segment|FAIL|" + err.Error())
		}

		//set snapshost status
		if len(s.chunks) > 0 {
			self.chunkId = s.chunks[len(s.chunks)-1].id
		}

	}
}

// query head data
func (self *MemorySnapshot) Head() (int64, []*Chunk) {
	self.RLock()
	defer self.RUnlock()
	var first *Segment
	if len(self.segments) > 0 {
		first = self.segments[0]
		//check cid in cache
		for e := self.segmentCache.Front(); nil != e; e = e.Next() {
			s := e.Value.(*Segment)
			if s.sid == first.sid {
				return first.sid, s.LoadChunks()
			}
		}

		//not in cache load into cache
		self.loadSegment(0)
		return first.sid, first.LoadChunks()

	}

	return -1, nil

}

//query one chunk by  chunkid
func (self *MemorySnapshot) Query(cid int64) *Chunk {

	curr := self.indexSegment(cid)
	if nil == curr {
		return nil
	}
	//find chunk
	return curr.Get(cid)
}

//index segment
func (self *MemorySnapshot) indexSegment(cid int64) *Segment {
	var curr *Segment
	self.RLock()
	//check cid in cache
	for e := self.segmentCache.Front(); nil != e; e = e.Next() {
		s := e.Value.(*Segment)
		if s.sid <= cid && cid <= (s.sid+int64(len(s.chunks))) {
			curr = s
		}
	}
	self.RUnlock()

	// not exist In cache
	if nil == curr {
		self.Lock()
		idx := sort.Search(len(self.segments), func(i int) bool {
			s := self.segments[i]
			return s.sid >= cid
		})

		if idx >= len(self.segments) || self.segments[idx].sid != cid {
			idx = idx - 1
		}

		//load segment
		self.loadSegment(idx)
		curr = self.segments[idx]
		self.Unlock()

	}
	return curr
}

//return the front chunk
func (self *MemorySnapshot) loadSegment(idx int) {

	// load n segments
	s := self.segments[idx]
	err := s.Open()
	if nil != err {
		log.Error("MemorySnapshot|loadSegment|FAIL|%s|%s\n", err, s.name)
		return
	} else {
		//pop header
		for e := self.segmentCache.Front(); self.segmentCache.Len() > self.segcacheSize; {
			self.segmentCache.Remove(e)
		}
		//push to cache
		self.segmentCache.PushBack(s)
	}
	log.Info("MemorySnapshot|loadSegment|SUCC|%s\n", s.name)
}

//mark delete
func (self *MemorySnapshot) Delete(cid int64) {
	s := self.indexSegment(cid)
	if nil != s {
		s.Delete(cid)
	} else {
		// log.Debug("MemorySnapshot|Delete|chunkid:%d|%s\n", cid, s)
	}
}

//write
func (self *MemorySnapshot) Append(msg []byte) int64 {

	if self.running {
		//create chunk
		chunk := &Chunk{
			length:   int32(CHUNK_HEADER + len(msg)),
			id:       self.cid(),
			checksum: crc32.ChecksumIEEE(msg),
			data:     msg,
			flag:     NORMAL}
		//write to channel for async flush
		self.writeChannel <- chunk
		return chunk.id
	} else {
		close(self.writeChannel)
		return -1
	}

}

func (self *MemorySnapshot) sync() {

	batch := make([]*Chunk, 0, self.batchSize)

	var popChunk *Chunk
	lastSeg := self.checkRoll()
	for self.running {

		//no batch / wait for data
		select {
		case popChunk = <-self.writeChannel:
		default:
			//no write data flush

		}

		if nil != popChunk {
			c := popChunk
			batch = append(batch, c)
		}

		//force flush
		if nil == popChunk && len(batch) > 0 || len(batch) >= cap(batch) {
			err := lastSeg.Append(batch)
			if nil != err {
				log.Error("MemorySnapshot|Append|FAIL|%s\n", err)
			}
			batch = batch[:0]
		}

		popChunk = nil
		//check roll
		lastSeg = self.checkRoll()
	}

	// need flush left data
outter:
	for {
		select {
		case chunk := <-self.writeChannel:
			if nil != chunk {
				batch = append(batch, chunk)
			}

		default:

			if len(batch) > 0 {
				//complete
				lastSeg.Append(batch)
				batch = batch[:0]
			}
			break outter
		}

	}

	self.waitSync.Done()
	log.Info("MemorySnapshot|SYNC|CLOSE...")
}

//check if
func (self *MemorySnapshot) checkRoll() *Segment {
	//if current segment bytesize is larger than max segment size
	//create a new segment for storage

	var s *Segment
	if len(self.segments) <= 0 {
		news, err := self.createSegment(self.chunkId + 1)
		if nil == err {
			self.Lock()
			//append new
			self.segments = append(self.segments, news)

			self.Unlock()
			s = news

		} else {
			//panic  first segment fail
			panic(err)
		}
	} else {
		self.RLock()
		s = self.segments[len(self.segments)-1]
		self.RUnlock()
		if s.byteSize > MAX_SEGMENT_SIZE {
			self.Lock()
			news, err := self.createSegment(self.chunkId + 1)
			if nil == err {
				//left segments are larger than cached ,close current
				if len(self.segments) >= self.segcacheSize {
					s.Close()
				}
				//append new
				self.segments = append(self.segments, news)
				s = news
			}
			self.Unlock()
		}
	}
	return s
}

//create segemnt
func (self *MemorySnapshot) createSegment(nextStart int64) (*Segment, error) {
	name := fmt.Sprintf("%s-%d", SEGMENT_PREFIX, nextStart) + SEGMENT_DATA_SUFFIX

	news := &Segment{
		path:     self.filePath + name,
		name:     name,
		sid:      nextStart,
		offset:   0,
		byteSize: 0}

	err := news.Open()
	if nil != err {
		log.Error("MemorySnapshot|currentSegment|Open Segment|FAIL|%s", news.path)
		return nil, err
	}
	return news, nil

}

//remove sid
func (self *MemorySnapshot) Remove(sid int64) {

	//check cid in cache
	for e := self.segmentCache.Front(); nil != e; e = e.Next() {
		s := e.Value.(*Segment)
		if s.sid == sid {
			s.Close()
			os.Remove(s.path)
			log.Info("MemorySnapshot|Remove|Segment|%s", s.path)
			break
		}
	}
}

func (self *MemorySnapshot) Destory() {
	self.running = false
	self.waitSync.Wait()
	//close all segment
	for _, s := range self.segments {
		err := s.Close()
		if nil != err {
			log.Error("MemorySnapshot|Destory|Close|FAIL|%s|sid:%d", err, s.sid)
		}
	}

	self.baseDir.Close()
	log.Info("MemorySnapshot|Destory...")

}

//chunk id
func (self *MemorySnapshot) cid() int64 {
	return atomic.AddInt64(&self.chunkId, 1)
}

func (self MemorySnapshot) String() string {
	return fmt.Sprintf("filePath:%s\tchunkid:%d\tsegments:%d",
		self.filePath, self.chunkId, len(self.segments))
}

// 检查目录是否存在
func dirExist(dir string) bool {
	info, err := os.Stat(dir)
	if err == nil {
		return info.IsDir()
	} else {
		return !os.IsNotExist(err) && info.IsDir()
	}
}