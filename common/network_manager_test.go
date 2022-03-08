package common

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/mailru/easyjson"
	"github.com/oxtoacart/bpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k6lib "go.k6.io/k6/lib"
	k6metrics "go.k6.io/k6/lib/metrics"
	k6testutils "go.k6.io/k6/lib/testutils"
	k6mockresolver "go.k6.io/k6/lib/testutils/mockresolver"
	k6types "go.k6.io/k6/lib/types"
	k6stats "go.k6.io/k6/stats"
)

const mockHostname = "host.test"

type fakeSession struct {
	cdpSession
	cdpCalls []string
}

// Execute implements the cdp.Executor interface to record calls made to it and
// allow assertions in tests.
func (s *fakeSession) Execute(ctx context.Context, method string,
	params easyjson.Marshaler, res easyjson.Unmarshaler) error {
	s.cdpCalls = append(s.cdpCalls, method)
	return nil
}

func newTestNetworkManager(t *testing.T, k6opts k6lib.Options) (*NetworkManager, *fakeSession) {
	ctx := context.Background()

	root, err := k6lib.NewGroup("", nil)
	require.NoError(t, err)

	state := &k6lib.State{
		Options:        k6opts,
		Logger:         k6testutils.NewLogger(t),
		Group:          root,
		BPool:          bpool.NewBufferPool(1),
		Samples:        make(chan k6stats.SampleContainer, 1000),
		Tags:           k6lib.NewTagMap(map[string]string{"group": root.Path}),
		BuiltinMetrics: k6metrics.RegisterBuiltinMetrics(k6metrics.NewRegistry()),
	}

	ctx = k6lib.WithState(ctx, state)
	logger := NewLogger(ctx, state.Logger, false, nil)

	session := &fakeSession{
		cdpSession: &Session{
			id: "1234",
		},
	}

	mr := k6mockresolver.New(map[string][]net.IP{
		mockHostname: {
			net.ParseIP("127.0.0.10"),
			net.ParseIP("127.0.0.11"),
			net.ParseIP("127.0.0.12"),
			net.ParseIP("2001:db8::10"),
			net.ParseIP("2001:db8::11"),
			net.ParseIP("2001:db8::12"),
		},
	}, nil)

	nm := &NetworkManager{
		ctx:      ctx,
		logger:   logger,
		session:  session,
		resolver: mr,
	}

	return nm, session
}

func TestOnRequestPausedBlockedHostnames(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name, reqURL                  string
		blockedHostnames, expCDPCalls []string
	}{
		{
			name:             "ok_fail_simple",
			blockedHostnames: []string{"*.test"},
			reqURL:           fmt.Sprintf("http://%s/", mockHostname),
			expCDPCalls:      []string{"Fetch.failRequest"},
		},
		{
			name:             "ok_continue_simple",
			blockedHostnames: []string{"*.test"},
			reqURL:           "http://host.com/",
			expCDPCalls:      []string{"Fetch.continueRequest"},
		},
		{
			name:             "ok_continue_empty",
			blockedHostnames: nil,
			reqURL:           "http://host.com/",
			expCDPCalls:      []string{"Fetch.continueRequest"},
		},
		{
			name:             "ok_continue_ip",
			blockedHostnames: []string{"*.test"},
			reqURL:           "http://127.0.0.1:8000/",
			expCDPCalls:      []string{"Fetch.continueRequest"},
		},
		{
			name:             "err_url_continue",
			blockedHostnames: []string{"*.test"},
			reqURL:           ":::",
			expCDPCalls:      []string{"Fetch.continueRequest"},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			blocked, err := k6types.NewNullHostnameTrie(tc.blockedHostnames)
			require.NoError(t, err)

			k6opts := k6lib.Options{BlockedHostnames: blocked}
			nm, session := newTestNetworkManager(t, k6opts)
			ev := &fetch.EventRequestPaused{
				RequestID: "1234",
				Request: &network.Request{
					Method: "GET",
					URL:    tc.reqURL,
				},
			}

			nm.onRequestPaused(ev)

			assert.Equal(t, tc.expCDPCalls, session.cdpCalls)
		})
	}
}

func TestOnRequestPausedBlockedIPs(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name, reqURL            string
		blockedIPs, expCDPCalls []string
	}{
		{
			name:        "ok_fail_simple",
			blockedIPs:  []string{"10.0.0.0/8", "192.168.0.0/16"},
			reqURL:      "http://10.0.0.1:8000/",
			expCDPCalls: []string{"Fetch.failRequest"},
		},
		{
			name:        "ok_fail_resolved_ip",
			blockedIPs:  []string{"127.0.0.10/32"},
			reqURL:      fmt.Sprintf("http://%s/", mockHostname),
			expCDPCalls: []string{"Fetch.failRequest"},
		},
		{
			name:        "ok_continue_resolved_ip",
			blockedIPs:  []string{"127.0.0.50/32"},
			reqURL:      fmt.Sprintf("http://%s/", mockHostname),
			expCDPCalls: []string{"Fetch.continueRequest"},
		},
		{
			name:        "ok_continue_simple",
			blockedIPs:  []string{"127.0.0.0/8"},
			reqURL:      "http://10.0.0.1:8000/",
			expCDPCalls: []string{"Fetch.continueRequest"},
		},
		{
			name:        "ok_continue_empty",
			blockedIPs:  nil,
			reqURL:      "http://127.0.0.1/",
			expCDPCalls: []string{"Fetch.continueRequest"},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			blockedIPs := make([]*k6lib.IPNet, len(tc.blockedIPs))
			for i, ipcidr := range tc.blockedIPs {
				ipnet, err := k6lib.ParseCIDR(ipcidr)
				require.NoError(t, err)
				blockedIPs[i] = ipnet
			}

			k6opts := k6lib.Options{BlacklistIPs: blockedIPs}
			nm, session := newTestNetworkManager(t, k6opts)
			ev := &fetch.EventRequestPaused{
				RequestID: "1234",
				Request: &network.Request{
					Method: "GET",
					URL:    tc.reqURL,
				},
			}

			nm.onRequestPaused(ev)

			assert.Equal(t, tc.expCDPCalls, session.cdpCalls)
		})
	}
}
