//go:build auditprobe

// SPDX-License-Identifier: GPL-3.0-or-later

// Package auditprobe holds the M0-04 audit probes for spec/21 AGT-12.
//
// The probes drive the existing (unmodified) kernel integrations with real
// VLESS-over-TCP connections and record whether the three kernel capabilities
// hold: credential add/remove does not disturb other connections, removal
// closes the removed credential's connections within 1 s, and per-credential
// byte counts equal the payload exactly.
//
// They are excluded from `make test` by the build tag because several of them
// are expected to fail on the unmodified fork (see FORK_PLAN.md §4). They are
// evidence for the audit, not the M3-06 conformance suite.
//
//	go test -tags auditprobe -count=1 -v ./internal/auditprobe/
package auditprobe

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/kernel/singbox"
	"github.com/cedar2025/xboard-node/internal/kernel/xray"
	"github.com/cedar2025/xboard-node/internal/model"
)

var (
	userA = model.UserSpec{ID: 1, UUID: "11111111-1111-4111-8111-111111111111"}
	userB = model.UserSpec{ID: 2, UUID: "22222222-2222-4222-8222-222222222222"}
	userC = model.UserSpec{ID: 3, UUID: "33333333-3333-4333-8333-333333333333"}
)

type kernelCase struct {
	name string
	make func() kernel.Kernel
}

func kernels() []kernelCase {
	return []kernelCase{
		{"singbox", func() kernel.Kernel {
			return singbox.New(config.KernelConfig{
				Type:     "singbox",
				LogLevel: "error",
				// The generated config blocks 127.0.0.0/8 (SSRF guard); let the
				// probe reach its local servers.
				CustomRoute: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}},
			})
		}},
		{"xray", func() kernel.Kernel {
			return xray.New(config.KernelConfig{
				Type:        "xray",
				LogLevel:    "error",
				CustomRoute: []map[string]any{{"type": "field", "ip": []string{"127.0.0.1/32"}, "outboundTag": "direct"}},
			})
		}},
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func echoServer(t *testing.T) *net.TCPAddr {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return l.Addr().(*net.TCPAddr)
}

// udpReplyServer answers every datagram of n bytes with a datagram of 3n bytes.
func udpReplyServer(t *testing.T) *net.UDPAddr {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(bytes.Repeat(buf[:n], 3), addr)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr)
}

func parseUUID(s string) []byte {
	out := make([]byte, 0, 16)
	hex := func(c byte) byte {
		switch {
		case c >= '0' && c <= '9':
			return c - '0'
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10
		}
		return 0
	}
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '-' {
			continue
		}
		out = append(out, hex(s[i])<<4|hex(s[i+1]))
		i++
	}
	return out
}

// vlessConn is a minimal VLESS client (no flow, no encryption) over plain TCP.
// Both kernels prefix the 2-byte response header to the first server write.
type vlessConn struct {
	net.Conn
	once   sync.Once
	hdrErr error
}

func (v *vlessConn) Read(b []byte) (int, error) {
	v.once.Do(func() {
		var h [2]byte
		if _, err := io.ReadFull(v.Conn, h[:]); err != nil {
			v.hdrErr = err
			return
		}
		if h[1] > 0 {
			_, v.hdrErr = io.CopyN(io.Discard, v.Conn, int64(h[1]))
		}
	})
	if v.hdrErr != nil {
		return 0, v.hdrErr
	}
	return v.Conn.Read(b)
}

// dialVLESS opens a VLESS request; cmd 1 = TCP, 2 = UDP (length-prefixed packets).
func dialVLESS(port int, uuid string, cmd byte, ip net.IP, targetPort int) (*vlessConn, error) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		return nil, err
	}
	var hdr bytes.Buffer
	hdr.WriteByte(0)           // version
	hdr.Write(parseUUID(uuid)) // user id
	hdr.WriteByte(0)           // addons length
	hdr.WriteByte(cmd)
	_ = binary.Write(&hdr, binary.BigEndian, uint16(targetPort))
	hdr.WriteByte(1) // address type: IPv4
	hdr.Write(ip.To4())
	if _, err := c.Write(hdr.Bytes()); err != nil {
		c.Close()
		return nil, err
	}
	return &vlessConn{Conn: c}, nil
}

func dialTCP(port int, uuid string, target *net.TCPAddr) (*vlessConn, error) {
	return dialVLESS(port, uuid, 1, target.IP, target.Port)
}

// roundTrip writes n random bytes and reads the n echoed bytes back.
func roundTrip(c *vlessConn, n int, timeout time.Duration) error {
	_ = c.SetDeadline(time.Now().Add(timeout))
	defer c.SetDeadline(time.Time{})
	payload := make([]byte, n)
	_, _ = rand.Read(payload)
	errCh := make(chan error, 1)
	go func() { _, err := c.Write(payload); errCh <- err }()
	got := make([]byte, n)
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if err := <-errCh; err != nil {
		return err
	}
	if !bytes.Equal(got, payload) {
		return errors.New("echo mismatch")
	}
	return nil
}

func traffic(t *testing.T, k kernel.Kernel) map[int][2]int64 {
	t.Helper()
	tr, _, _, err := k.GetUserTraffic(t.Context())
	if err != nil {
		t.Fatalf("GetUserTraffic: %v", err)
	}
	return tr
}

// waitTraffic polls until the counter for uid reaches want or d passes.
func waitTraffic(t *testing.T, k kernel.Kernel, uid int, want [2]int64, d time.Duration) [2]int64 {
	t.Helper()
	deadline := time.Now().Add(d)
	var got [2]int64
	for time.Now().Before(deadline) {
		got = traffic(t, k)[uid]
		if got == want {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	return got
}

// closedWithin reports whether the connection is observed closed within d.
// Each iteration round-trips one byte; a read timeout counts as "not closed".
func closedWithin(c *vlessConn, d time.Duration) (bool, time.Duration) {
	start := time.Now()
	for time.Since(start) < d {
		if err := roundTrip(c, 1, 200*time.Millisecond); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return true, time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, time.Since(start)
}

func vlessSpec(port int) *model.NodeSpec {
	return &model.NodeSpec{Protocol: "vless", ServerPort: port, Network: "tcp"}
}

func startKernel(t *testing.T, kc kernelCase, spec *model.NodeSpec, users []model.UserSpec) kernel.Kernel {
	t.Helper()
	k := kc.make()
	if err := k.Start(spec, users, kernel.TLSCert{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(k.Stop)
	time.Sleep(200 * time.Millisecond)
	return k
}

// ─── probes ─────────────────────────────────────────────────────────────────

// TestProbeMeteringExact: AGT-12 (3) – per-credential bytes equal the payload
// exactly (known-size payload, VLESS/TCP, no mux).
func TestProbeMeteringExact(t *testing.T) {
	for _, kc := range kernels() {
		t.Run(kc.name, func(t *testing.T) {
			echo := echoServer(t)
			port := freePort(t)
			k := startKernel(t, kc, vlessSpec(port), []model.UserSpec{userA, userB})

			c, err := dialTCP(port, userA.UUID, echo)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			const n = 256 * 1024
			for i := 0; i < 4; i++ {
				if err := roundTrip(c, n, 5*time.Second); err != nil {
					t.Fatalf("round trip: %v", err)
				}
			}
			want := [2]int64{4 * n, 4 * n}
			got := waitTraffic(t, k, userA.ID, want, 2*time.Second)
			t.Logf("RESULT %s TCP metering open conn: want=%v got=%v", kc.name, want, got)
			if got != want {
				t.Errorf("metering mismatch while open: want %v got %v", want, got)
			}
			c.Close()
			got = waitTraffic(t, k, userA.ID, want, 2*time.Second)
			t.Logf("RESULT %s TCP metering after close: want=%v got=%v", kc.name, want, got)
			if got != want {
				t.Errorf("metering mismatch after close: want %v got %v", want, got)
			}
			if b := traffic(t, k)[userB.ID]; b != ([2]int64{}) {
				t.Errorf("bytes attributed to idle user B: %v", b)
			}
		})
	}
}

// TestProbeUDPDirection: AGT-12 (3) for UDP – VLESS UDP, client sends
// 10 × 100 B, server replies 10 × 300 B. Expected: up = 1000, down = 3000.
func TestProbeUDPDirection(t *testing.T) {
	for _, kc := range kernels() {
		t.Run(kc.name, func(t *testing.T) {
			ua := udpReplyServer(t)
			port := freePort(t)
			k := startKernel(t, kc, vlessSpec(port), []model.UserSpec{userA})

			vc, err := dialVLESS(port, userA.UUID, 2, ua.IP, ua.Port)
			if err != nil {
				t.Fatal(err)
			}
			defer vc.Close()
			_ = vc.SetDeadline(time.Now().Add(5 * time.Second))
			payload := bytes.Repeat([]byte{0x5a}, 100)
			for i := 0; i < 10; i++ {
				var pkt bytes.Buffer
				_ = binary.Write(&pkt, binary.BigEndian, uint16(len(payload)))
				pkt.Write(payload)
				if _, err := vc.Write(pkt.Bytes()); err != nil {
					t.Fatal(err)
				}
				var l [2]byte
				if _, err := io.ReadFull(vc, l[:]); err != nil {
					t.Fatalf("read len: %v", err)
				}
				if _, err := io.CopyN(io.Discard, vc, int64(binary.BigEndian.Uint16(l[:]))); err != nil {
					t.Fatalf("read pkt: %v", err)
				}
			}
			want := [2]int64{1000, 3000}
			got := waitTraffic(t, k, userA.ID, want, 2*time.Second)
			t.Logf("RESULT %s UDP up/down want=%v got=%v", kc.name, want, got)
			if got != want {
				t.Errorf("UDP metering: want %v got %v", want, got)
			}
		})
	}
}

// TestProbeAddRemoveIsolation: AGT-12 (1) – adding C and removing A leaves B's
// established connection working, and B and C can open new connections.
func TestProbeAddRemoveIsolation(t *testing.T) {
	for _, kc := range kernels() {
		t.Run(kc.name, func(t *testing.T) {
			echo := echoServer(t)
			port := freePort(t)
			k := startKernel(t, kc, vlessSpec(port), []model.UserSpec{userA, userB})

			cb, err := dialTCP(port, userB.UUID, echo)
			if err != nil {
				t.Fatal(err)
			}
			defer cb.Close()
			if err := roundTrip(cb, 1024, 3*time.Second); err != nil {
				t.Fatalf("B initial: %v", err)
			}

			if _, err := k.AddUsers([]model.UserSpec{userC}); err != nil {
				t.Fatalf("add C: %v", err)
			}
			errAdd := roundTrip(cb, 1024, 3*time.Second)
			if _, err := k.RemoveUsers([]model.UserSpec{userA}); err != nil {
				t.Fatalf("remove A: %v", err)
			}
			errRemove := roundTrip(cb, 1024, 3*time.Second)
			t.Logf("RESULT %s B after add C: err=%v; after remove A: err=%v", kc.name, errAdd, errRemove)
			if errAdd != nil || errRemove != nil {
				t.Errorf("B's connection disturbed: add=%v remove=%v", errAdd, errRemove)
			}

			cc, errC := dialTCP(port, userC.UUID, echo)
			if errC == nil {
				errC = roundTrip(cc, 1024, 3*time.Second)
				cc.Close()
			}
			nb, errB := dialTCP(port, userB.UUID, echo)
			if errB == nil {
				errB = roundTrip(nb, 1024, 3*time.Second)
				nb.Close()
			}
			t.Logf("RESULT %s new conn C: err=%v; new conn B: err=%v", kc.name, errC, errB)
			if errC != nil || errB != nil {
				t.Errorf("new connections failed: C=%v B=%v", errC, errB)
			}
		})
	}
}

// TestProbeRemoveClosesSessions: AGT-12 (2) – after RemoveUsers(A) plus the
// explicit CloseUserConnections hook, A's established connection must close
// within 1 s and a new connection with A's credential must be refused. Also
// records whether bytes A moves after removal are still counted.
func TestProbeRemoveClosesSessions(t *testing.T) {
	for _, kc := range kernels() {
		t.Run(kc.name, func(t *testing.T) {
			echo := echoServer(t)
			port := freePort(t)
			k := startKernel(t, kc, vlessSpec(port), []model.UserSpec{userA, userB})

			ca, err := dialTCP(port, userA.UUID, echo)
			if err != nil {
				t.Fatal(err)
			}
			defer ca.Close()
			if err := roundTrip(ca, 1000, 3*time.Second); err != nil {
				t.Fatalf("A initial: %v", err)
			}
			before := waitTraffic(t, k, userA.ID, [2]int64{1000, 1000}, 2*time.Second)

			if _, err := k.RemoveUsers([]model.UserSpec{userA}); err != nil {
				t.Fatalf("remove A: %v", err)
			}
			_ = k.CloseUserConnections(t.Context(), userA.UUID)

			closed, after := closedWithin(ca, time.Second)
			t.Logf("RESULT %s existing conn of removed A closed within 1s: %v (%v)", kc.name, closed, after.Round(time.Millisecond))
			if !closed {
				t.Errorf("removed credential's connection still alive after 1s")
				if err := roundTrip(ca, 5000, 3*time.Second); err == nil {
					tr := traffic(t, k)
					t.Logf("RESULT %s A counters before removal=%v, after 5000 B post-removal=%v (present=%v)",
						kc.name, before, tr[userA.ID], tr[userA.ID] != [2]int64{})
				}
			}

			na, err := dialTCP(port, userA.UUID, echo)
			if err == nil {
				err = roundTrip(na, 16, 2*time.Second)
				na.Close()
			}
			t.Logf("RESULT %s new conn with removed A refused: %v (err=%v)", kc.name, err != nil, err)
			if err == nil {
				t.Errorf("new connection with removed credential succeeded")
			}
		})
	}
}

// TestProbeReloadMetering: a route-only configuration change while B has an
// open connection. Records whether B's connection survives and whether B's
// bytes stay exact across the reload.
func TestProbeReloadMetering(t *testing.T) {
	for _, kc := range kernels() {
		t.Run(kc.name, func(t *testing.T) {
			echo := echoServer(t)
			port := freePort(t)
			spec := vlessSpec(port)
			users := []model.UserSpec{userA, userB}
			k := startKernel(t, kc, spec, users)

			cb, err := dialTCP(port, userB.UUID, echo)
			if err != nil {
				t.Fatal(err)
			}
			defer cb.Close()
			if err := roundTrip(cb, 10000, 3*time.Second); err != nil {
				t.Fatalf("B initial: %v", err)
			}
			waitTraffic(t, k, userB.ID, [2]int64{10000, 10000}, 2*time.Second)

			spec2 := *spec
			spec2.Routes = []model.RouteRule{{ID: 1, Match: []string{"example.invalid"}, Action: "block"}}
			if err := k.Reload(&spec2, users, kernel.TLSCert{}); err != nil {
				t.Fatalf("reload: %v", err)
			}
			time.Sleep(300 * time.Millisecond)

			errAfter := roundTrip(cb, 20000, 3*time.Second)
			want := [2]int64{10000, 10000}
			if errAfter == nil {
				want = [2]int64{30000, 30000}
			}
			got := waitTraffic(t, k, userB.ID, want, 2*time.Second)
			t.Logf("RESULT %s route-only reload: B conn survives=%v (err=%v); B counters want=%v got=%v",
				kc.name, errAfter == nil, errAfter, want, got)
			if got != want {
				t.Errorf("metering across reload: want %v got %v", want, got)
			}
		})
	}
}
