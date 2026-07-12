/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config

import (
	"testing"

	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

func TestNewUsesExplicitSectionDecoders(t *testing.T) {
	sections, err := config_parser.Parse(`
global {
  log_level: info
  so_mark_from_dae: 1234
}

subscription {
  "https://example.com/sub"
}

node {
  "ss://example"
}

group {
  proxy {
    policy: random
    filter: name(keyword: hk)
  }
}

routing {
  pname(NetworkManager) -> direct
  fallback: proxy
}

dns {
  ipversion_prefer: 6
  upstream {
    google:"8.8.8.8:53"
  }
  routing {
    request {
      qname(geosite:geolocation-!cn) -> proxy
      fallback: direct
    }
    response {
      fallback: proxy
    }
  }
}
`)
	require.NoError(t, err)

	conf, err := New(sections)
	require.NoError(t, err)
	require.True(t, conf.Global.SoMarkFromDaeSet)
	require.Len(t, conf.Subscription, 1)
	require.Len(t, conf.Node, 1)
	require.Len(t, conf.Group, 1)
	require.Equal(t, "proxy", conf.Group[0].Name)
	require.Equal(t, 6, conf.Dns.IpVersionPrefer)
	require.NotNil(t, conf.Routing.Fallback)
	require.NotNil(t, conf.Dns.Routing.Request.Fallback)
	require.NotNil(t, conf.Dns.Routing.Response.Fallback)
}

func TestGlobalMemoryDefaults(t *testing.T) {
	sections, err := config_parser.Parse(`
global {}
routing {
  fallback: direct
}
`)
	require.NoError(t, err)

	conf, err := New(sections)
	require.NoError(t, err)
	require.True(t, conf.Global.DisableTHP)
	require.EqualValues(t, 262144, conf.Global.BpfConnStateMapSize)
}

// TestGlobalHealthCheckFieldsOptInByDefault guards the opt-in health-check
// design: tcp_check_url / udp_check_dns / check_interval carry no `default:`
// tag in config.go, so omitting them leaves them at their zero value. If
// someone re-introduces upstream-style defaults that auto-enable checks, this
// test fails and surfaces the regression instead of silently forcing health
// checks on for every node.
func TestGlobalHealthCheckFieldsOptInByDefault(t *testing.T) {
	sections, err := config_parser.Parse(`
global {}
routing {
  fallback: direct
}
`)
	require.NoError(t, err)

	conf, err := New(sections)
	require.NoError(t, err)
	require.Empty(t, conf.Global.TcpCheckUrl, "tcp_check_url must default to empty (opt-in)")
	require.Empty(t, conf.Global.UdpCheckDns, "udp_check_dns must default to empty (opt-in)")
	require.Zero(t, conf.Global.CheckInterval, "check_interval must default to zero (opt-in)")
}

func TestDecodeConfigSectionRejectsUnknownSection(t *testing.T) {
	conf := &Config{}
	err := decodeConfigSection(conf, "unknown", &config_parser.Section{Name: "unknown"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown section")
}
