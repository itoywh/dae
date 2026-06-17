/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package logger

import (
	"time"

	"github.com/sirupsen/logrus"
	prefixed "github.com/x-cray/logrus-prefixed-formatter"
	"gopkg.in/natefinch/lumberjack.v2"
)

// cstLocation is the CST (UTC+8) timezone used for log timestamps.
// On minimal OpenWrt/ImmortalWrt without tzdata, we use a fixed offset.
var cstLocation *time.Location

func init() {
	// Initialize CST location without modifying global time.Local.
	// This ensures other packages' time operations are unaffected.
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		cstLocation = loc
	} else {
		cstLocation = time.FixedZone("CST", 8*3600)
	}
}

// cstFormatter wraps prefixed.TextFormatter to use CST timezone for timestamps.
type cstFormatter struct {
	*prefixed.TextFormatter
}

// Format overrides the timestamp formatting to use CST timezone.
func (f *cstFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	// Convert entry.Time to CST before formatting
	if !f.DisableTimestamp && entry.Time != (time.Time{}) {
		entry.Time = entry.Time.In(cstLocation)
	}
	return f.TextFormatter.Format(entry)
}

func SetLogger(log *logrus.Logger, logLevel string, disableTimestamp bool, logFileOpt *lumberjack.Logger) {
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}

	log.SetLevel(level)
	log.SetFormatter(&cstFormatter{
		TextFormatter: &prefixed.TextFormatter{
			DisableTimestamp: disableTimestamp,
			FullTimestamp:    true,
			ForceFormatting:  true,
			TimestampFormat:  "2006-01-02 15:04:05 CST",
		},
	})
	if logFileOpt != nil {
		log.SetOutput(logFileOpt)
	}
}
