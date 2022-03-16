// Copyright 2019 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"os"
	"io/ioutil"
	"fmt"
	"crypto/md5"
	"encoding/binary"
	"sync"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// Coordinator coordinates Alertmanager configurations beyond the lifetime of a
// single configuration.
type Coordinator struct {
	configFilePath string
	logger         log.Logger

	// Protects config and subscribers
	mutex       sync.Mutex
	config      *Config
	subscribers []func(*Config) error

	configHashMetric        prometheus.Gauge
	configSuccessMetric     prometheus.Gauge
	configSuccessTimeMetric prometheus.Gauge

	// a mutex for disk writes
	diskLock	sync.Mutex
}

// NewCoordinator returns a new coordinator with the given configuration file
// path. It does not yet load the configuration from file. This is done in
// `Reload()`.
func NewCoordinator(configFilePath string, r prometheus.Registerer, l log.Logger) *Coordinator {
	c := &Coordinator{
		configFilePath: configFilePath,
		logger:         l,
	}

	c.registerMetrics(r)

	return c
}

func (c *Coordinator) registerMetrics(r prometheus.Registerer) {
	configHash := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "alertmanager_config_hash",
		Help: "Hash of the currently loaded alertmanager configuration.",
	})
	configSuccess := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "alertmanager_config_last_reload_successful",
		Help: "Whether the last configuration reload attempt was successful.",
	})
	configSuccessTime := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "alertmanager_config_last_reload_success_timestamp_seconds",
		Help: "Timestamp of the last successful configuration reload.",
	})

	r.MustRegister(configHash, configSuccess, configSuccessTime)

	c.configHashMetric = configHash
	c.configSuccessMetric = configSuccess
	c.configSuccessTimeMetric = configSuccessTime
}

// Subscribe subscribes the given Subscribers to configuration changes.
func (c *Coordinator) Subscribe(ss ...func(*Config) error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.subscribers = append(c.subscribers, ss...)
}

func (c *Coordinator) notifySubscribers() error {
	for _, s := range c.subscribers {
		if err := s(c.config); err != nil {
			return err
		}
	}

	return nil
}

// loadFromFile triggers a configuration load, discarding the old configuration.
func (c *Coordinator) loadFromFile() error {
	conf, err := LoadFile(c.configFilePath)
	if err != nil {
		return err
	}

	c.config = conf

	return nil
}

// AddRouteAndReceiver updates a new receiver and route to 
// disk (config file) 
func (c *Coordinator) AddRouteAndReceiver(rr *RouteAndReceiver) error {
	
	c.diskLock.Lock()
	defer c.diskLock.Unlock()

	if len(c.config.original) == 0 {
		return fmt.Errorf("can not update to an empty config from the disk")
	}

	// load original config from disk 
	configFromDisk, err := Load(c.config.original)
	if err != nil {
		return err
	}
	
	configFromDisk.Global.SMTPFrom="amol@gmail.com"

	return c.saveUpdatesToDisk(configFromDisk.String())
}

func (c *Coordinator) saveUpdatesToDisk(s string) error {
	
	// open config file to get perm_mode and create backup
	f, err := os.Open(c.configFilePath); 
	if err != nil {
		return err
	}

	// close the file now that we know it exists 
	defer f.Close()

	// fetching the perm here as it is mandatory for writeFile 
	// ops. The ioutil.writefile uses the perm to create file 
	// if it does not exist. But in our case it is unlikely 
	// to ever happen
	fileInfo, _ := f.Stat()f
	
	// fetching perm for backup file 
	perm := fileInfo.Mode().Perm()

	// todo: backup the current config file 
	// open in os.OpenFile(dst, syscall.O_CREATE | syscall.O_EXCL, FileMode(0666))
	
	fmt.Println("permissions:", perm)

	return ioutil.WriteFile(c.configFilePath, []byte(s), perm)
}

// Reload triggers a configuration reload from file and notifies all
// configuration change subscribers.
func (c *Coordinator) Reload() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	level.Info(c.logger).Log(
		"msg", "Loading configuration file",
		"file", c.configFilePath,
	)
	if err := c.loadFromFile(); err != nil {
		level.Error(c.logger).Log(
			"msg", "Loading configuration file failed",
			"file", c.configFilePath,
			"err", err,
		)
		c.configSuccessMetric.Set(0)
		return err
	}
	level.Info(c.logger).Log(
		"msg", "Completed loading of configuration file",
		"file", c.configFilePath,
	)

	if err := c.notifySubscribers(); err != nil {
		c.logger.Log(
			"msg", "one or more config change subscribers failed to apply new config",
			"file", c.configFilePath,
			"err", err,
		)
		c.configSuccessMetric.Set(0)
		return err
	}

	c.configSuccessMetric.Set(1)
	c.configSuccessTimeMetric.SetToCurrentTime()
	hash := md5HashAsMetricValue([]byte(c.config.original))
	c.configHashMetric.Set(hash)

	return nil
}

func md5HashAsMetricValue(data []byte) float64 {
	sum := md5.Sum(data)
	// We only want 48 bits as a float64 only has a 53 bit mantissa.
	smallSum := sum[0:6]
	var bytes = make([]byte, 8)
	copy(bytes, smallSum)
	return float64(binary.LittleEndian.Uint64(bytes))
}
