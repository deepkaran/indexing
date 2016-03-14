// Copyright (c) 2014 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package indexer

import (
	"fmt"
	"github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/logging"
	"strings"
	"time"
)

type CompactionManager interface {
}

type compactionManager struct {
	logPrefix string
	config    common.Config
	supvMsgCh MsgChannel
	supvCmdCh MsgChannel
}

type compactionDaemon struct {
	quitch  chan bool
	started bool
	timer   *time.Timer
	msgch   MsgChannel
	config  common.ConfigHolder
	clusterAddr string
}

func (cd *compactionDaemon) Start() {
	if !cd.started {
		conf := cd.config.Load()
		dur := time.Second * time.Duration(conf["check_period"].Int())
		cd.timer = time.NewTimer(dur)
		cd.started = true
		go cd.loop()
	}
}

func (cd *compactionDaemon) Stop() {
	if cd.started {
		cd.timer.Stop()
		cd.quitch <- true
		<-cd.quitch
	}
}

func (cd *compactionDaemon) ResetConfig(c common.Config) {
	cd.config.Store(c)
}

func (cd *compactionDaemon) needsCompaction(is IndexStorageStats, config common.Config) bool {

	mode := strings.ToLower(config["compaction_mode"].String())
	logging.Infof("CompactionDaemon: Checking fragmentation: %s, mode : %s", is.String(), mode)

	if mode == "full" {
		// if full compaction, then
		// 1) check min_size
		// 2) check min_frag
		if uint64(is.Stats.DiskSize) > config["min_size"].Uint64() {
			if is.GetFragmentation() >= float64(config["min_frag"].Int()) {
				return true
			}
		}
	} else {
		// if circular compaction, then
		// 1) check compaction interval
		// 2) check the week of day
		interval := config["interval"].String()
		isCompactionInterval := true
		if interval != "00:00,00:00" {
			var start_hr, start_min, end_hr, end_min int
			n, err := fmt.Sscanf(interval, "%d:%d,%d:%d", &start_hr, &start_min, &end_hr, &end_min)
			start_min += start_hr * 60
			end_min += end_hr * 60

			if n == 4 && err == nil {
				hr, min, _ := time.Now().Clock()
				min += hr * 60

				if min < start_min || min > end_min {
					isCompactionInterval = false
				}
			}
		}

		if !isCompactionInterval {
			logging.Infof("CompactionDaemon: Compaction attempt skipped since compaction interval is configured for %v", interval)
			return false
		}

		hasDaysOfWeek := false
		days := config["days_of_week"].Strings()
		today := strings.ToLower(time.Now().Weekday().String())
		for _, day := range days {
			if strings.ToLower(strings.TrimSpace(day)) == today {
				return true
			}
			hasDaysOfWeek = true
		}

		if hasDaysOfWeek {
			logging.Infof("CompactionDaemon: Compaction attempt skipped since compaction day is configured for %v", days)
		}
	}

	return false
}

func (cd *compactionDaemon) loop() {
	var stats []IndexStorageStats
loop:
	for {
		select {
		case _, ok := <-cd.timer.C:

			conf := cd.config.Load()
			if common.GetStorageMode() == common.FORESTDB {

				if ok {
					replych := make(chan []IndexStorageStats)
					statReq := &MsgIndexStorageStats{respch: replych}
					cd.msgch <- statReq
					stats = <-replych

					for _, is := range stats {
						needUpgrade := is.Stats.NeedUpgrade
						if needUpgrade || cd.needsCompaction(is, conf) {
							errch := make(chan error)
							compactReq := &MsgIndexCompact{
								instId: is.InstId,
								errch:  errch,
							}
							logging.Infof("CompactionDaemon: Compacting index instance:%v", is.InstId)
							if needUpgrade {
								common.Console(cd.clusterAddr, "Compacting index %v.%v for upgrade", is.Bucket, is.Name)
							}
							cd.msgch <- compactReq
							err := <-errch
							if err == nil {
								logging.Infof("CompactionDaemon: Finished compacting index instance:%v", is.InstId)
								if needUpgrade {
									common.Console(cd.clusterAddr, "Finished compacting index %v.%v for upgrade", is.Bucket, is.Name)
								}
							} else {
								logging.Errorf("CompactionDaemon: Index instance:%v Compaction failed with reason - %v", is.InstId, err)
								if needUpgrade {
									common.Console(cd.clusterAddr, "Compaction for index %v.%v failed with reason - %v", is.Bucket, is.Name, err)
								}
							}
						}
					}
				}
			}

			dur := time.Second * time.Duration(conf["check_period"].Int())
			cd.timer.Reset(dur)

		case <-cd.quitch:
			cd.quitch <- true
			break loop
		}
	}
}

func NewCompactionManager(supvCmdCh MsgChannel, supvMsgCh MsgChannel,
	config common.Config) (CompactionManager, Message) {
	cm := &compactionManager{
		config:    config,
		supvCmdCh: supvCmdCh,
		supvMsgCh: supvMsgCh,
		logPrefix: "CompactionManager",
	}
	go cm.run()
	return cm, &MsgSuccess{}
}

func (cm *compactionManager) run() {
	cd := cm.newCompactionDaemon()
	cd.Start()
loop:
	for {
		select {
		case cmd, ok := <-cm.supvCmdCh:
			if ok {
				if cmd.GetMsgType() == COMPACTION_MGR_SHUTDOWN {
					logging.Infof("%v: Shutting Down", cm.logPrefix)
					cm.supvCmdCh <- &MsgSuccess{}
					break loop
				} else if cmd.GetMsgType() == CONFIG_SETTINGS_UPDATE {
					logging.Infof("%v: Refreshing settings", cm.logPrefix)
					cfgUpdate := cmd.(*MsgConfigUpdate)
					fullConfig := cfgUpdate.GetConfig()
					cfg := fullConfig.SectionConfig("settings.compaction.", true)
					cd.ResetConfig(cfg)
					cm.supvCmdCh <- &MsgSuccess{}
				}
			} else {
				break loop
			}
		}
	}

	cd.Stop()
}

func (cm *compactionManager) newCompactionDaemon() *compactionDaemon {
	cfg := cm.config.SectionConfig("settings.compaction.", true)
	clusterAddr := cm.config["clusterAddr"].String()
	cd := &compactionDaemon{
		quitch:  make(chan bool),
		started: false,
		msgch:   cm.supvMsgCh,
		clusterAddr: clusterAddr,
	}
	cd.config.Store(cfg)

	return cd
}
