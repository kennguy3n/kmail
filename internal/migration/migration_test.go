package migration

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The validation paths in every Create* method and the
// state-transition checks in StartJob / CancelJob return before
// touching the pool, so a nil-pool Service is sufficient for
// these input-validation unit tests.

func newTestService() *Service {
	return NewService(Config{
		Pool:          nil,
		MaxConcurrent: 2,
		Now:           func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
}

// ------------------------------------------------------------------
// Input validation
// ------------------------------------------------------------------

func TestCreateJob_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		in   CreateJobInput
	}{
		{"empty", CreateJobInput{}},
		{"no source host", CreateJobInput{
			SourceUser:     "u",
			SourcePassword: "p",
			DestUser:       "d",
		}},
		{"no source user", CreateJobInput{
			SourceHost:     "imap.example.com",
			SourcePassword: "p",
			DestUser:       "d",
		}},
		{"no source password", CreateJobInput{
			SourceHost: "imap.example.com",
			SourceUser: "u",
			DestUser:   "d",
		}},
		{"no dest user", CreateJobInput{
			SourceHost:     "imap.example.com",
			SourceUser:     "u",
			SourcePassword: "p",
		}},
	}
	svc := newTestService()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateJob(context.Background(), "tid", tc.in)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestCreateJob_MissingTenantID(t *testing.T) {
	_, err := newTestService().CreateJob(context.Background(), "", CreateJobInput{
		SourceHost:     "imap.example.com",
		SourceUser:     "u",
		SourcePassword: "p",
		DestUser:       "d",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestGetJob_EmptyJobID(t *testing.T) {
	_, err := newTestService().GetJob(context.Background(), "tid", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty job id, got %v", err)
	}
}

// ------------------------------------------------------------------
// Lifecycle helpers
// ------------------------------------------------------------------

func TestMigrationJob_Terminal(t *testing.T) {
	cases := map[string]bool{
		"pending":   false,
		"running":   false,
		"paused":    false,
		"completed": true,
		"failed":    true,
		"cancelled": true,
	}
	for status, want := range cases {
		t.Run(status, func(t *testing.T) {
			j := &MigrationJob{Status: status}
			if got := j.Terminal(); got != want {
				t.Errorf("Terminal() for %q = %v, want %v", status, got, want)
			}
		})
	}
}

// ------------------------------------------------------------------
// Password encoding round-trip
// ------------------------------------------------------------------

func TestEncryptDecryptPassword_RoundTrip(t *testing.T) {
	enc, err := encryptPassword("hunter2")
	if err != nil {
		t.Fatalf("encryptPassword: %v", err)
	}
	if enc == "hunter2" {
		t.Errorf("encryptPassword returned plaintext")
	}
	got, err := decryptPassword(enc)
	if err != nil {
		t.Fatalf("decryptPassword: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("round-trip: got %q, want %q", got, "hunter2")
	}
}

func TestDecryptPassword_RejectsUnknownEncoding(t *testing.T) {
	_, err := decryptPassword("not-a-kmail-enc")
	if err == nil {
		t.Fatal("expected error for unknown encoding")
	}
}

func TestEncryptPassword_EmptyInput(t *testing.T) {
	enc, err := encryptPassword("")
	if err != nil {
		t.Fatalf("encryptPassword: %v", err)
	}
	if enc != "" {
		t.Errorf("empty input should produce empty ciphertext, got %q", enc)
	}
}

// ------------------------------------------------------------------
// Progress regex
// ------------------------------------------------------------------

func TestImapsyncProgressRegex(t *testing.T) {
	line := "++++ Statistics : Folder [INBOX] Messages 1234 of 2345 done"
	m := imapsyncProgressRE.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("expected regex to match: %q", line)
	}
	if m[1] != "1234" || m[2] != "2345" {
		t.Errorf("captures = %v, want [1234 2345]", m[1:])
	}
}

func TestImapsyncProgressRegex_NoMatch(t *testing.T) {
	line := "some unrelated imapsync output"
	if m := imapsyncProgressRE.FindStringSubmatch(line); m != nil {
		t.Errorf("expected no match, got %v", m)
	}
}

// ------------------------------------------------------------------
// NewService defaults
// ------------------------------------------------------------------

func TestNewService_AppliesDefaults(t *testing.T) {
	s := NewService(Config{})
	if s.cfg.MaxConcurrent <= 0 {
		t.Errorf("MaxConcurrent default not applied")
	}
	if s.cfg.ImapsyncBin == "" {
		t.Errorf("ImapsyncBin default not applied")
	}
	if s.cfg.Now == nil {
		t.Errorf("Now default not applied")
	}
	if s.sema == nil {
		t.Errorf("sema not initialised")
	}
	if s.cancels == nil {
		t.Errorf("cancels map not initialised")
	}
}

// ------------------------------------------------------------------
// TestConnection
// ------------------------------------------------------------------

func TestTestConnection_ValidationErrors(t *testing.T) {
	svc := newTestService()
	cases := []struct {
		name string
		in   TestConnectionInput
	}{
		{"no host", TestConnectionInput{Port: 993, Username: "u", Password: "p"}},
		{"bad port", TestConnectionInput{Host: "h", Port: 0, Username: "u", Password: "p"}},
		{"no user", TestConnectionInput{Host: "h", Port: 993, Password: "p"}},
		{"no pass", TestConnectionInput{Host: "h", Port: 993, Username: "u"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.TestConnection(context.Background(), tc.in)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

// fakeIMAPServer replays a scripted IMAP exchange so we can exercise
// TestConnection without touching the network. It accepts a single
// connection, sends the configured greeting, then returns OK / NO
// responses for each tagged command depending on `acceptLogin`.
type fakeIMAPServer struct {
	listener    net.Listener
	acceptLogin bool
	greeting    string
	wg          sync.WaitGroup
}

func newFakeIMAPServer(t *testing.T, acceptLogin bool) *fakeIMAPServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeIMAPServer{
		listener:    l,
		acceptLogin: acceptLogin,
		greeting:    "* OK fake-imap ready\r\n",
	}
	srv.wg.Add(1)
	go srv.serve()
	t.Cleanup(func() {
		_ = l.Close()
		srv.wg.Wait()
	})
	return srv
}

func (s *fakeIMAPServer) addr() (host string, port int) {
	a := s.listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (s *fakeIMAPServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeIMAPServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(s.greeting)); err != nil {
		return
	}
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			return
		}
		tag, cmd := fields[0], strings.ToUpper(fields[1])
		switch cmd {
		case "LOGIN":
			if s.acceptLogin {
				_, _ = conn.Write([]byte(tag + " OK LOGIN completed\r\n"))
			} else {
				_, _ = conn.Write([]byte(tag + " NO authentication failed\r\n"))
			}
		case "LOGOUT":
			_, _ = conn.Write([]byte("* BYE goodbye\r\n"))
			_, _ = conn.Write([]byte(tag + " OK LOGOUT completed\r\n"))
			return
		default:
			_, _ = conn.Write([]byte(tag + " BAD unsupported\r\n"))
		}
	}
}

func TestTestConnection_LoginSuccess(t *testing.T) {
	srv := newFakeIMAPServer(t, true)
	host, port := srv.addr()
	err := newTestService().TestConnection(context.Background(), TestConnectionInput{
		Host:     host,
		Port:     port,
		Username: "alice",
		Password: "hunter2",
		UseTLS:   false,
	})
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
}

func TestTestConnection_LoginRejected(t *testing.T) {
	srv := newFakeIMAPServer(t, false)
	host, port := srv.addr()
	err := newTestService().TestConnection(context.Background(), TestConnectionInput{
		Host:     host,
		Port:     port,
		Username: "alice",
		Password: "wrong",
	})
	if err == nil {
		t.Fatal("expected login rejection")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("expected NO line in error, got %v", err)
	}
}

func TestTestConnection_DialFailure(t *testing.T) {
	// Listener bound to :0 then immediately closed gives us a port
	// that is guaranteed not to be listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().(*net.TCPAddr)
	_ = l.Close()
	err = newTestService().TestConnection(context.Background(), TestConnectionInput{
		Host:     "127.0.0.1",
		Port:     addr.Port,
		Username: "u",
		Password: "p",
	})
	if err == nil {
		t.Fatal("expected dial error")
	}
	if !strings.Contains(err.Error(), "dial") {
		t.Errorf("expected dial error wrapper, got %v", err)
	}
}

func TestQuoteIMAP(t *testing.T) {
	cases := map[string]string{
		"":              `""`,
		"alice":         `"alice"`,
		`a"b`:           `"a\"b"`,
		`back\slash`:    `"back\\slash"`,
		"unicode-ünder": `"unicode-ünder"`,
	}
	for in, want := range cases {
		if got := quoteIMAP(in); got != want {
			t.Errorf("quoteIMAP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImapCommand_RejectsUnexpectedCompletion(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	go func() {
		br := bufio.NewReader(server)
		_, _ = br.ReadString('\n')
		_, _ = server.Write([]byte("a1 WEIRD whatever\r\n"))
	}()
	br := bufio.NewReader(client)
	if err := imapCommand(client, br, "a1", "NOOP"); err == nil ||
		!strings.Contains(err.Error(), "unexpected completion") {
		t.Errorf("expected unexpected-completion error, got %v", err)
	}
	// strconv import keeps the build green even if the smoke
	// helpers above are removed in future cleanup.
	_ = strconv.Itoa(0)
}
