package sender

import (
	"bytes"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/qiniu/log"
	"github.com/qiniu/logkit/conf"
	"github.com/qiniu/logkit/queue"
	"github.com/qiniu/logkit/utils"
)

const (
	mb                = 1024 * 1024 // 1MB
	defaultWriteLimit = 10          // 默认写速限制为10MB
	maxBytesPerFile   = 100 * mb
	qNameSuffix       = "_local_save"
	defaultMaxProcs   = 1 // 默认没有并发
)

// 可选参数 fault_tolerant 为true的话，以下必填
const (
	KeyFtSyncEvery   = "ft_sync_every"    // 该参数设置多少次写入会同步一次offset log
	KeyFtSaveLogPath = "ft_save_log_path" // disk queue 数据日志路径
	KeyFtWriteLimit  = "ft_write_limit"   // 写入速度限制，单位MB
	KeyFtStrategy    = "ft_strategy"      // ft 的策略
	KeyFtProcs       = "ft_procs"         // ft并发数，当always_save 策略时启用
)

// ft 策略
const (
	// KeyFtStrategyBackupOnly 只在失败的时候进行容错
	KeyFtStrategyBackupOnly = "backup_only"
	// KeyFtStrategyAlwaysSave 所有数据都进行容错
	KeyFtStrategyAlwaysSave = "always_save"
)

// FtSender fault tolerance sender wrapper
type FtSender struct {
	stopped     int32
	exitChan    chan struct{}
	innerSender Sender
	logQueue    queue.BackendQueue
	backupQueue queue.BackendQueue
	writeLimit  int  // 写入速度限制，单位MB
	backupOnly  bool // 是否只使用backup queue
	procs       int  //发送并发数
	se          *utils.StatsError
}

type datasContext struct {
	Datas []Data `json:"datas"`
}

// NewFtSender Fault tolerant sender constructor
func NewFtSender(sender Sender, conf conf.MapConf) (*FtSender, error) {
	logpath, err := conf.GetString(KeyFtSaveLogPath)
	if err != nil {
		return nil, err
	}
	syncEvery, _ := conf.GetIntOr(KeyFtSyncEvery, DefaultFtSyncEvery)
	writeLimit, _ := conf.GetIntOr(KeyFtWriteLimit, defaultWriteLimit)
	strategy, _ := conf.GetStringOr(KeyFtStrategy, KeyFtStrategyAlwaysSave)
	procs, _ := conf.GetIntOr(KeyFtProcs, defaultMaxProcs)
	return newFtSender(sender, logpath, int64(syncEvery), writeLimit, strategy == KeyFtStrategyBackupOnly, procs)
}

func newFtSender(innerSender Sender, saveLogPath string, syncEvery int64, writeLimit int, backupOnly bool, procs int) (*FtSender, error) {
	err := utils.CreateDirIfNotExist(saveLogPath)
	if err != nil {
		return nil, err
	}

	lq := queue.NewDiskQueue("stream"+qNameSuffix, saveLogPath, maxBytesPerFile, 0, maxBytesPerFile, syncEvery, syncEvery, time.Second*2, writeLimit*mb)
	bq := queue.NewDiskQueue("backup"+qNameSuffix, saveLogPath, maxBytesPerFile, 0, maxBytesPerFile, syncEvery, syncEvery, time.Second*2, writeLimit*mb)
	ftSender := FtSender{
		exitChan:    make(chan struct{}),
		innerSender: innerSender,
		logQueue:    lq,
		backupQueue: bq,
		writeLimit:  writeLimit,
		backupOnly:  backupOnly,
		procs:       procs,
		se:          &utils.StatsError{Ft: true},
	}
	go ftSender.asyncSendLogFromDiskQueue()
	return &ftSender, nil
}

func (ft *FtSender) Name() string {
	return ft.innerSender.Name() + "(ft)"
}

func (ft *FtSender) Send(datas []Data) error {
	if ft.backupOnly {
		// 尝试直接发送数据，当数据失败的时候会加入到本地重试队列。外部不需要重试
		err := ft.trySendDatas(datas, 1)
		if err != nil {
			log.Warn(ft.innerSender.Name() + " trySendDatas err" + err.Error())
			ft.se.AddErrors()
		} else {
			ft.se.AddSuccess()
		}
		// 容错队列会保证重试，此处不向外部暴露发送错误信息
		ft.se.ErrorDetail = nil
		ft.se.Ftlag = ft.backupQueue.Depth()
	} else {
		err := ft.saveToFile(datas)
		if err != nil {
			return err
		}
		ft.se.Ftlag = ft.backupQueue.Depth() + ft.logQueue.Depth()
		ft.se.ErrorDetail = nil
	}
	return ft.se
}

func (ft *FtSender) Close() error {
	atomic.AddInt32(&ft.stopped, 1)
	log.Warn("wait for sender " + ft.Name() + " completely exit")
	// 等待错误恢复流程退出
	<-ft.exitChan
	// 等待正常发送流程退出
	for i := 0; i < ft.procs; i++ {
		<-ft.exitChan
	}

	log.Warn(ft.Name() + " has been completely exited")

	// persist queue's meta data
	ft.logQueue.Close()
	ft.backupQueue.Close()

	return ft.innerSender.Close()
}

// marshalData 将数据序列化
func (ft *FtSender) marshalData(datas []Data) (bs []byte, err error) {
	ctx := new(datasContext)
	ctx.Datas = datas
	bs, err = json.Marshal(ctx)
	if err != nil {
		err = NewSendError("Cannot marshal data :"+err.Error(), datas, TypeDefault)
		return
	}
	return
}

// unmarshalData 如何将数据从磁盘中反序列化出来
func (ft *FtSender) unmarshalData(dat []byte) (datas []Data, err error) {
	ctx := new(datasContext)
	d := json.NewDecoder(bytes.NewReader(dat))
	d.UseNumber()
	err = d.Decode(&ctx)
	if err != nil {
		return
	}
	datas = ctx.Datas
	return
}

func (ft *FtSender) saveToFile(datas []Data) error {
	bs, err := ft.marshalData(datas)
	if err != nil {
		return err
	}
	err = ft.logQueue.Put(bs)
	if err != nil {
		return NewSendError(ft.innerSender.Name()+" Cannot put data into diskqueue :"+err.Error(), datas, TypeDefault)
	}
	return nil
}

func (ft *FtSender) asyncSendLogFromDiskQueue() {
	for i := 0; i < ft.procs; i++ {
		go ft.sendFromStreamQueue()
	}
	go ft.retryFromBackupQueue()
}

// trySend 从bytes反序列化数据后尝试发送数据
func (ft *FtSender) trySendBytes(dat []byte, failSleep int) (err error) {
	datas, err := ft.unmarshalData(dat)
	if err != nil {
		return
	}
	return ft.trySendDatas(datas, failSleep)
}

// trySendDatas 尝试发送数据，如果失败，将失败数据加入backup queue，并睡眠指定时间。返回结果为是否正常发送
func (ft *FtSender) trySendDatas(datas []Data, failSleep int) (err error) {
	err = ft.innerSender.Send(datas)
	if c, ok := err.(*utils.StatsError); ok {
		err = c.ErrorDetail
	}
	if err != nil {
		log.Errorf("%s cannot write points + %v", ft.innerSender.Name(), err)
		failCtx := new(datasContext)
		var binaryUnpack bool
		se, succ := err.(*SendError)
		if !succ {
			// 如果不是SendError 默认所有的数据都发送失败
			log.Infof("error type is not *SendError! reSend all datas by default")
			failCtx.Datas = datas
		} else {
			failCtx.Datas = se.failDatas
			if se.ErrorType == TypeBinaryUnpack {
				binaryUnpack = true
			}
		}
		if binaryUnpack {
			lens := len(failCtx.Datas) / 2
			if lens > 0 {
				newFailCtx := new(datasContext)
				newFailCtx.Datas = failCtx.Datas[0:lens]
				failCtx.Datas = failCtx.Datas[lens:]
				nnBytes, _ := json.Marshal(newFailCtx)
				ft.backupQueue.Put(nnBytes)
			}
		}
		newBytes, _ := json.Marshal(failCtx)
		ft.backupQueue.Put(newBytes)
		time.Sleep(time.Second * time.Duration(failSleep))
	}
	return
}

func (ft *FtSender) sendFromStreamQueue() {
	readChan := ft.logQueue.ReadChan()
	timer := time.NewTicker(time.Second)
	for {
		if atomic.LoadInt32(&ft.stopped) > 0 {
			ft.exitChan <- struct{}{}
			return
		}
		select {
		case dat := <-readChan:
			err := ft.trySendBytes(dat, 1)
			if err != nil {
				log.Errorf("%s cannot send points from queue %v, error %v", ft.innerSender.Name(), ft.logQueue.Name(), err)
				ft.se.AddErrors()
			} else {
				ft.se.AddSuccess()
			}
		case <-timer.C:
			continue
		}
	}
}

func (ft *FtSender) retryFromBackupQueue() {
	readChan := ft.backupQueue.ReadChan()
	timer := time.NewTicker(time.Second)
	waitCnt := 1
	for {
		if atomic.LoadInt32(&ft.stopped) > 0 {
			ft.exitChan <- struct{}{}
			return
		}
		select {
		case dat := <-readChan:
			err := ft.trySendBytes(dat, waitCnt)
			if err == nil {
				waitCnt = 1
				ft.se.AddSuccess()
			} else {
				log.Errorf("%s cannot send points from queue %v, error is %v", ft.innerSender.Name(), ft.backupQueue.Name(), err)
				ft.se.AddErrors()
				waitCnt++
				if waitCnt > 10 {
					waitCnt = 10
				}
			}
		case <-timer.C:
			continue
		}
	}
}
