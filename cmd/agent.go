// This file is part of bizfly-backup
//
// Copyright (C) 2020  BizFly Cloud
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v3"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/bizflycloud/bizfly-backup/pkg/backupapi"
	"github.com/bizflycloud/bizfly-backup/pkg/broker/mqtt"
	"github.com/bizflycloud/bizfly-backup/pkg/server"
)

var defaultAddr = "unix://" + filepath.Join(os.TempDir(), "bizfly-backup.sock")

// agentCmd represents the agent command
var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run agent.",
	Run: func(cmd *cobra.Command, args []string) {
		machineID := viper.GetString("machine_id")
		accessKey := viper.GetString("access_key")
		secretKey := viper.GetString("secret_key")
		apiUrl := viper.GetString("api_url")
		backupClient, err := backupapi.NewClient(
			backupapi.WithAccessKey(accessKey),
			backupapi.WithSecretKey(secretKey),
			backupapi.WithServerURL(apiUrl),
			backupapi.WithID(machineID),
		)
		if err != nil {
			logger.Error("failed to create new backup client", zap.Error(err))
			os.Exit(1)
		}
		bo := backoff.WithMaxRetries(backoff.NewConstantBackOff(3*time.Second), 3)
		var brokerUrl string
		for {
			umr, err := backupClient.UpdateMachine()
			if err == nil {
				brokerUrl = umr.BrokerUrl
				break
			}
			logger.Error("failed to update machine info", zap.Error(err))
			d := bo.NextBackOff()
			if d == backoff.Stop {
				os.Exit(1)
			}
			time.Sleep(d)
		}

		mqttUrl := brokerUrl
		agentID := machineID
		b, err := mqtt.NewBroker(
			mqtt.WithURL(mqttUrl),
			mqtt.WithClientID(agentID),
			mqtt.WithUsername(accessKey),
			mqtt.WithPassword(secretKey),
		)
		if err != nil {
			logger.Fatal("failed to create broker", zap.Error(err))
			os.Exit(1)
		}

		logger.Debug("Listening address: " + addr)
		s, err := server.New(
			server.WithAddr(addr),
			server.WithBroker(b),
			server.WithSubscribeTopics("agent/default", "agent/"+agentID),
			server.WithPublishTopic("agent/"+agentID),
			server.WithBackupClient(backupClient),
		)
		if err != nil {
			logger.Fatal("failed to create new server", zap.Error(err))
			os.Exit(1)
		}
		if err := s.Run(); !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("server run failed", zap.Error(err))
			os.Exit(1)
		}
	},
}

var agentVersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version of agent server.",
	Run: func(cmd *cobra.Command, args []string) {
		httpc := http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", strings.TrimPrefix(addr, "unix://"))
				},
			},
		}

		req, err := http.NewRequest(http.MethodPost, "http://unix/version", nil)
		if err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}
		resp, err := httpc.Do(req)
		if err != nil {
			logger.Error(err.Error())
			os.Exit(1)
		}
		defer resp.Body.Close()
		b, _ := ioutil.ReadAll(resp.Body)
		fmt.Println(string(b))
	},
}

func init() {
	rootCmd.AddCommand(agentCmd)
	agentCmd.AddCommand(agentVersionCmd)
}
