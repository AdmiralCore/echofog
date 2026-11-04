package carrier

import (
	"context"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	d := dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) { return nil, nil })
	r.RegisterDialer(objects.SuiteNoise3, d)
	if got, ok := r.DialerFor(objects.SuiteNoise3); !ok || got == nil {
		t.Fatal("DialerFor did not return the registered dialer")
	}
	if _, ok := r.DialerFor(objects.SuiteReality2QUIC); ok {
		t.Fatal("DialerFor returned a dialer for an unregistered suite")
	}

	l := testListener{}
	r.RegisterListener(objects.SuiteReality2TCP, l)
	if got, ok := r.ListenerFor(objects.SuiteReality2TCP); !ok || got == nil {
		t.Fatal("ListenerFor did not return the registered listener")
	}
	if _, ok := r.ListenerFor(objects.SuiteWS2); ok {
		t.Fatal("ListenerFor returned a listener for an unregistered suite")
	}
}

func TestRegistryPanicsOnBadRegistration(t *testing.T) {
	cases := []struct {
		name string
		f    func(*Registry)
	}{
		{"nil dialer", func(r *Registry) { r.RegisterDialer(objects.SuiteNoise3, nil) }},
		{"suite zero dialer", func(r *Registry) {
			r.RegisterDialer(0, dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) { return nil, nil }))
		}},
		{"duplicate dialer", func(r *Registry) {
			d := dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) { return nil, nil })
			r.RegisterDialer(objects.SuiteNoise3, d)
			r.RegisterDialer(objects.SuiteNoise3, d)
		}},
		{"nil listener", func(r *Registry) { r.RegisterListener(objects.SuiteWS2, nil) }},
		{"suite zero listener", func(r *Registry) { r.RegisterListener(0, testListener{}) }},
		{"duplicate listener", func(r *Registry) {
			r.RegisterListener(objects.SuiteWS2, testListener{})
			r.RegisterListener(objects.SuiteWS2, testListener{})
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			r := NewRegistry()
			defer func() {
				if recover() == nil {
					t.Fatal("registration did not panic")
				}
			}()
			c.f(r)
		})
	}
}

func TestDefaultRegistry(t *testing.T) {
	d := dialFunc(func(context.Context, DialRequest) (mux.Carrier, error) { return nil, nil })
	RegisterDialer(0xff01, d)
	if got, ok := DialerFor(0xff01); !ok || got == nil {
		t.Fatal("package-level RegisterDialer/DialerFor do not use the default registry")
	}
	l := testListener{}
	RegisterListener(0xff02, l)
	if got, ok := ListenerFor(0xff02); !ok || got == nil {
		t.Fatal("package-level RegisterListener/ListenerFor do not use the default registry")
	}
}
