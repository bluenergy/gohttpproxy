package proxyutil

import (
	"github.com/gohttpproxy/gohttpproxy/martian/constants"
	"github.com/gohttpproxy/gohttpproxy/martian/log"
	"golang.org/x/exp/slices"
	"net"
	"sync"
	"time"
)

const CLEAN_SIZE = 32
const HF_SIZE = 32

type PuHelper struct {
	FMap   map[string]*PoolConn[net.Conn]
	HFList []string
	FMux   sync.RWMutex
}

func NewPuHelper() *PuHelper {

	var FMap = make(map[string]*PoolConn[net.Conn])
	var HFList = make([]string, HF_SIZE)
	var FMux sync.RWMutex

	return &PuHelper{
		FMap:   FMap,
		HFList: HFList,
		FMux:   FMux,
	}
}

func (ph *PuHelper) CleanPu(destStr string) {
	ph.FMux.Lock()
	defer ph.FMux.Unlock()
	if _, ok := ph.FMap[destStr]; ok {

		FMSIZE := len(ph.FMap)

		var ch = make(chan string, FMSIZE)

		var tmpMap = make(map[string]*PoolConn[net.Conn])

		for i, _ := range ph.FMap {

			//并发检测 元素是否应该保留
			go func(ch chan string, i string) {

				if i != destStr && slices.Contains(ph.HFList, i) {
					ch <- i

				} else {
					ch <- ""
				}

			}(ch, i)
		}

		for j := 0; j < FMSIZE; j++ {
			vr := <-ch
			if vr != "" {
				tmpMap[vr] = ph.FMap[vr]
			}
		}

		ph.FMap = tmpMap

		log.Infof("成功清理FMap, 清理后: %+v", ph.FMap)
	}
}

func (ph *PuHelper) GetOrCreatePu(destStr string, f func() (net.Conn, error)) *PoolConn[net.Conn] {

	if nil == ph.GetPu(destStr) {
		ph.CreatePu(destStr, f)
	}
	return ph.GetPu(destStr)
}

func (ph *PuHelper) GetPu(destStr string) *PoolConn[net.Conn] {
	ph.FMux.RLock()
	defer ph.FMux.RUnlock()
	log.Infof("FMap: %+v", ph.FMap)
	if val, ok := ph.FMap[destStr]; ok {
		go func() {

			if !slices.Contains(ph.HFList, destStr) {
				if len(ph.HFList) > HF_SIZE {
					tmpHFList := ph.HFList[len(ph.HFList)-HF_SIZE:]
					tmpHFList = append(tmpHFList, destStr)
					ph.HFList = tmpHFList
				} else {
					ph.HFList = append(ph.HFList, destStr)
				}
			}
		}()
		if val.FailedCount.Load() > constants.MAX_PD_ERROR {
			ph.CleanPu(destStr)
			return nil
		}
		return val
	}
	return nil
}

func (ph *PuHelper) CreatePu(destStr string, f func() (net.Conn, error)) {
	ph.FMux.Lock()
	defer ph.FMux.Unlock()

	go ph.TidyPu(destStr)

	if _, ok := ph.FMap[destStr]; !ok {

		tmpItem := NewPoolConnWithOptions[net.Conn](constants.MAX_CH_SIZE, 2, 2, 45*time.Millisecond)
		tmpItem.RegisterDialer(f)
		ph.FMap[destStr] = tmpItem
	}

}

func (ph *PuHelper) TidyPu(destStr string) {
	cm := "TidyPu@puhelper.go"
	ph.FMux.Lock()
	defer ph.FMux.Unlock()

	FMSIZE := len(ph.FMap)
	if FMSIZE >= CLEAN_SIZE {

		log.Infof("HFList: %+v", ph.HFList)

		var ch = make(chan string, FMSIZE)

		var tmpMap = make(map[string]*PoolConn[net.Conn])

		for i, _ := range ph.FMap {

			//并发检测 元素是否应该保留
			go func(ch chan string, i string) {

				if i == destStr || slices.Contains(ph.HFList, i) {
					log.Infof(cm+" dest: %v 被保留", i)
					ch <- i

				} else {
					ch <- ""
				}

			}(ch, i)
		}

		for j := 0; j < FMSIZE; j++ {
			vr := <-ch
			if vr != "" {
				tmpMap[vr] = ph.FMap[vr]
			}
		}

		ph.FMap = tmpMap

		log.Infof(" 清理完之后: %+v", ph.FMap)
	}
}
