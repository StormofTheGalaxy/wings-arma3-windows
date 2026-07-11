package windows

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pterodactyl/wings/environment"
)

func newTestEnvironment(t *testing.T, envVars []string) *Environment {
	t.Helper()
	e, err := New("test", &Metadata{Root: t.TempDir()}, environment.NewConfiguration(environment.Settings{}, envVars))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSplitParams(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"-enableHT -hugePages", []string{"-enableHT", "-hugePages"}},
		{`-mod="@some mod;@other" -noPause`, []string{"-mod=@some mod;@other", "-noPause"}},
		{"\t-bandwidthAlg=2\n-noSound", []string{"-bandwidthAlg=2", "-noSound"}},
	}
	for _, c := range cases {
		if got := splitParams(c.raw); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitParams(%q) = %#v, want %#v", c.raw, got, c.want)
		}
	}
}

func TestServerParamsLowLevelAndCustom(t *testing.T) {
	e := newTestEnvironment(t, []string{
		"SERVER_CPUCOUNT=4",
		"SERVER_EXTHREADS=7",
		"SERVER_MALLOC=tbb4malloc_bi",
		"SERVER_MAXMEM=8192",
		"SERVER_PARAMS=-enableHT -hugePages",
	})
	params := strings.Join(e.serverParams(), " ")
	for _, want := range []string{"-cpuCount=4", "-exThreads=7", "-malloc=tbb4malloc_bi", "-maxMem=8192", "-enableHT", "-hugePages", "-config=server.cfg"} {
		if !strings.Contains(params, want) {
			t.Errorf("server params missing %q: %s", want, params)
		}
	}
	if strings.Contains(params, "-par=") {
		t.Errorf("server params must not use a -par file: %s", params)
	}
}

func TestHCParamsCustom(t *testing.T) {
	e := newTestEnvironment(t, []string{
		"HC_CPUCOUNT=2",
		"HC_MAXMEM=4096",
		"HC_PARAMS=-noSound -world=empty",
		"SERVER_PASSWORD=secret",
	})
	params := e.hcParams(3)
	joined := strings.Join(params, " ")
	for _, want := range []string{"-client", "-name=hc-3", "-cpuCount=2", "-maxMem=4096", "-noSound", "-world=empty", "-password=secret"} {
		if !strings.Contains(joined, want) {
			t.Errorf("hc params missing %q: %s", want, joined)
		}
	}
	redacted := strings.Join(redactParams(params), " ")
	if strings.Contains(redacted, "secret") {
		t.Errorf("redactParams leaked password: %s", redacted)
	}
	if !strings.Contains(redacted, "-password=REDACTED") {
		t.Errorf("redactParams did not mask password: %s", redacted)
	}
}
