package proxy

import (
	"errors"
	"reflect"
	"testing"

	utls "github.com/refraction-networking/utls"
)

func alpnOf(spec *utls.ClientHelloSpec) []string {
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			return alpn.AlpnProtocols
		}
	}
	return nil
}

func TestUtlsDialerHelloSpec(t *testing.T) {
	chromeSpec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		t.Fatalf("UTLSIdToSpec(Chrome) error = %v", err)
	}

	tests := []struct {
		name    string
		dialer  *utlsDialer
		wantErr bool
		check   func(t *testing.T, spec *utls.ClientHelloSpec)
	}{
		{
			name:   "nil factory keeps Chrome ciphers with h1 ALPN",
			dialer: &utlsDialer{},
			check: func(t *testing.T, spec *utls.ClientHelloSpec) {
				t.Helper()
				if !reflect.DeepEqual(spec.CipherSuites, chromeSpec.CipherSuites) {
					t.Errorf("ciphers = %v, want Chrome's %v", spec.CipherSuites, chromeSpec.CipherSuites)
				}
				if got := alpnOf(spec); !reflect.DeepEqual(got, []string{"http/1.1"}) {
					t.Errorf("ALPN = %v, want [http/1.1]", got)
				}
			},
		},
		{
			name: "factory spec wins and ALPN is pinned without mutating input",
			dialer: &utlsDialer{specFactory: func() (*utls.ClientHelloSpec, error) {
				s, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
				if err != nil {
					return nil, err
				}
				s.CipherSuites = []uint16{utls.TLS_AES_128_GCM_SHA256}
				return &s, nil
			}},
			check: func(t *testing.T, spec *utls.ClientHelloSpec) {
				t.Helper()
				if !reflect.DeepEqual(spec.CipherSuites, []uint16{utls.TLS_AES_128_GCM_SHA256}) {
					t.Errorf("ciphers = %v, want factory's single suite", spec.CipherSuites)
				}
				if got := alpnOf(spec); !reflect.DeepEqual(got, []string{"http/1.1"}) {
					t.Errorf("ALPN = %v, want [http/1.1]", got)
				}
			},
		},
		{
			name:    "factory error propagates",
			dialer:  &utlsDialer{specFactory: func() (*utls.ClientHelloSpec, error) { return nil, errors.New("boom") }},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := tt.dialer.helloSpec()
			if (err != nil) != tt.wantErr {
				t.Fatalf("helloSpec() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			tt.check(t, spec)
		})
	}

	// pinALPNH1 must not mutate its input: a factory handing out a shared
	// spec stays safe to reuse across handshakes.
	t.Run("pinALPNH1 does not mutate input", func(t *testing.T) {
		orig, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
		if err != nil {
			t.Fatalf("UTLSIdToSpec error = %v", err)
		}
		before := alpnOf(&orig)
		pinned := pinALPNH1(orig)
		if !reflect.DeepEqual(alpnOf(pinned), []string{"http/1.1"}) {
			t.Errorf("pinned ALPN = %v, want [http/1.1]", alpnOf(pinned))
		}
		if !reflect.DeepEqual(alpnOf(&orig), before) {
			t.Errorf("input ALPN mutated: before %v, after %v", before, alpnOf(&orig))
		}
	})
}
