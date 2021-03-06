/* Copyright 2015 LinkedIn Corp. Licensed under the Apache License, Version
 * 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 */

package main

import (
	"fmt"
	"github.com/samuel/go-zookeeper/zk"
	log "github.com/cihub/seelog"
	"strconv"
	"time"
)

type ZooKeeperOffsetClient struct {
	app             	*ApplicationContext
	cluster            	string
	conn    			*zk.Conn
	zkOffsetTicker 		*time.Ticker
}

func NewZooKeeperOffsetClient(app *ApplicationContext, cluster string) (*ZooKeeperOffsetClient, error) {
	zkhosts := make([]string, len(app.Config.Kafka[cluster].Zookeepers))
	for i, host := range app.Config.Kafka[cluster].Zookeepers {
		zkhosts[i] = fmt.Sprintf("%s:%v", host, app.Config.Kafka[cluster].ZookeeperPort)
	}
	zkconn, _, err := zk.Connect(zkhosts, time.Duration(app.Config.Zookeeper.Timeout)*time.Second)
	if err != nil {
		return nil, err
	}

	client := &ZooKeeperOffsetClient{
		app:     app,
		cluster: cluster,
		conn:    zkconn,
	}

	// Now get the first set of offsets and start a goroutine to continually check them
	client.getOffsets(client.app.Config.Kafka[cluster].ZookeeperOffsetPaths)
	client.zkOffsetTicker = time.NewTicker(time.Duration(client.app.Config.Tickers.ZooKeeperOffsets) * time.Second)
	go func() {
		for _ = range client.zkOffsetTicker.C {
			client.getOffsets(client.app.Config.Kafka[cluster].ZookeeperOffsetPaths)
		}
	}()

	return client, nil
}

func (zkOffsetClient *ZooKeeperOffsetClient) getOffsetsForConsumerGroup(consumerGroup string, consumerGroupPath string) {
	topicsPath := consumerGroupPath + "/offsets"
	topics, _, err := zkOffsetClient.conn.Children(topicsPath)
	switch {
	case err == nil:
		for _, topic := range topics {
			zkOffsetClient.getOffsetsForTopic(consumerGroup, topic, topicsPath + "/" + topic)
		}
	case err ==  zk.ErrNoNode:
		// it is OK as the offsets may not be managed by ZK
		log.Debugf("This consumer group's offset is not managed by ZK: " + consumerGroup)
	default:
		log.Warnf("Failed to read topics for consumer group %s in ZK path %s", consumerGroup, consumerGroupPath + "/offsets")
	}
}

func (zkOffsetClient *ZooKeeperOffsetClient) getOffsetsForTopic(consumerGroup string, topic string, topicPath string) {
	partitions, _, err := zkOffsetClient.conn.Children(topicPath)
	switch {
	case err == nil:
		for _, partitionStr := range partitions {
			partition, errConversion := strconv.Atoi(partitionStr)
			switch {
			case errConversion == nil:
				zkOffsetClient.getOffsetForPartition(consumerGroup, topic,  partition, topicPath + "/" + partitionStr)
			default:
				log.Errorf("Something is very wrong! The partition %s for topic %s in consumer group %s in ZK path %s should be a number",
					partitionStr, topic, consumerGroup, topicPath)
			}
		}
	default:
		log.Warnf("Failed to read partitions for topic %s in consumer group %s in ZK path %s", topic, consumerGroup, topicPath)
	}
}

func (zkOffsetClient *ZooKeeperOffsetClient) getOffsetForPartition(consumerGroup string, topic string, partition int, partitionPath string) {
	zkNodeStat := &zk.Stat {}
	offsetStr, zkNodeStat, err := zkOffsetClient.conn.Get(partitionPath)
	switch {
	case err == nil:
		offset, errConversion := strconv.Atoi(string(offsetStr))
		switch {
		case errConversion == nil:
			log.Debugf("About to sync ZK based offset: [%s,%s,%v]::[%v,%v]\n", consumerGroup, topic, partition, offset, zkNodeStat.Mtime)
			partitionOffset := &PartitionOffset{
				Cluster:   zkOffsetClient.cluster,
				Topic:     topic,
				Partition: int32(partition),
				Group:     consumerGroup,
				Timestamp: int64(zkNodeStat.Mtime), // note: this is millis
				Offset:    int64(offset),
			}
			timeoutSendOffset(zkOffsetClient.app.Storage.offsetChannel, partitionOffset, 1)
		default:
			log.Errorf("Something is very wrong! The offset %s for partition %s for topic %s in consumer group %s in ZK path %s should be a number",
				offsetStr, partition, topic, consumerGroup, partitionPath)
		}
	default:
		log.Warnf("Failed to read partition for partition %s of topic %s in consumer group %s in ZK path %s", partition, topic, consumerGroup, partitionPath)
	}
}

func (zkOffsetClient *ZooKeeperOffsetClient) getOffsets(paths []string) {

	log.Infof("Start to refresh ZK based offsets stored in paths: %s", paths)
	// for now, we will perform the offset refreshing sequentially to keep it simple
	for _, path := range paths {

		// note: if a node does not exist, the "exists" flag will be set to false. The err, however, will be nil
		exists, _, err := zkOffsetClient.conn.Exists(path)
		switch {
		case err == nil:
			if !exists {
				// we don't tolerate configuration error
				log.Errorf("Invalid ZK offset path %s in configuration.", path)
				panic(err)
			}

			consumerGroups, _, err := zkOffsetClient.conn.Children(path)
			switch {
			case err == nil:
				for _, consumerGroup := range consumerGroups {
					zkOffsetClient.getOffsetsForConsumerGroup(consumerGroup, path + "/" + consumerGroup)
				}
			default:
				log.Warnf("Failed to read consumer groups in ZK path %s", path)
			}

		default:
			panic(err)
		}
	}
}

func (zkOffsetClient *ZooKeeperOffsetClient) Stop() {
	zkOffsetClient.conn.Close()
}
