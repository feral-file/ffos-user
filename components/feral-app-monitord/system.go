package main

import (
	"context"
	"fmt"
	"time"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/metric"
	"go.uber.org/zap"
)

func GetConnectivityStatus() (bool, error) {
	logger.Info("Getting connectivity status")

	deadlineCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := dbusClient.Call(
		deadlineCtx,
		MONITORD_DBUS_NAME,
		MONITORD_DBUS_PATH,
		MONITORD_DBUS_INTERFACE,
		MONITORD_METHOD_GET_CONNECTIVITY_STATUS,
		true,
	)
	logger.Debug("Connectivity status", zap.Any("resp", resp), zap.Error(err))
	if err != nil {
		return false, err
	}

	if len(resp) != 1 {
		return false, fmt.Errorf("expected 1 response, got %d", len(resp))
	}

	connected, ok := resp[0].(bool)
	if !ok {
		return false, fmt.Errorf("expected bool, got %T", resp[0])
	}

	return connected, nil
}

// GetSysMetrics retrieves system metrics from the sysmonitord service.
func GetSysMetrics() (*metric.SysDBusMetrics, error) {
	logger.Info("Getting system metrics")

	deadlineCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var metrics metric.SysDBusMetrics

	err := dbusClient.Query(
		deadlineCtx,
		&metrics,
		MONITORD_DBUS_NAME,
		MONITORD_DBUS_PATH,
		MONITORD_DBUS_INTERFACE,
		MONITORD_METHOD_GET_SYSMETRICS,
	)
	if err != nil {
		return nil, err
	}

	return &metrics, nil
}

type Page string

const (
	PageNone          Page = "None"
	PageQRCode        Page = "QRCode"
	PageMessage       Page = "Message"
	PageSystemUpgrade Page = "SystemUpgrade"
	PageFactoryReset  Page = "FactoryReset"
	PageWebApp        Page = "WebApp"
)

type PageState struct {
	ID              string `json:"id"`
	Page            Page   `json:"page"`
	PageChangedUnix int64  `json:"page_changed_unix"`
}

// GetSysMetrics retrieves system metrics from the sysmonitord service.
func GetPageState() (*PageState, error) {
	logger.Info("Getting page state")

	deadlineCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var pg PageState

	err := dbusClient.Scan(
		deadlineCtx,
		[]interface{}{&pg.ID, &pg.Page, &pg.PageChangedUnix},
		SETUPD_DBUS_NAME,
		SETUPD_DBUS_PATH,
		SETUPD_DBUS_INTERFACE,
		SETUPD_METHOD_GET_PAGE_STATE,
	)
	if err != nil {
		logger.Error("Getting page state error", zap.Error(err))
		return nil, err
	}
	logger.Info("Getting page state:", zap.Any("pg", pg))

	return &pg, nil
}
