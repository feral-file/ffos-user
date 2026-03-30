package devicectl

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDdcPanelAction(t *testing.T) {
	t.Parallel()

	valid := []string{
		"brightness", "BRIGHTNESS", "  contrast ", "volume", "mute", "power",
		string(DdcPanelActionBrightness),
	}
	for _, s := range valid {
		s := s
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			a, err := ParseDdcPanelAction(s)
			require.NoError(t, err)
			assert.NotEmpty(t, a)
		})
	}

	_, err := ParseDdcPanelAction("gamma")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid ddcPanelControl action")
}

func TestParseDdcMuteSetting(t *testing.T) {
	t.Parallel()

	m, err := ParseDdcMuteSetting("ON")
	require.NoError(t, err)
	assert.Equal(t, DdcMuteOn, m)

	m, err = ParseDdcMuteSetting("off")
	require.NoError(t, err)
	assert.Equal(t, DdcMuteOff, m)

	_, err = ParseDdcMuteSetting("maybe")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid mute value")
}

func TestParseDdcPowerSetting(t *testing.T) {
	t.Parallel()

	p, err := ParseDdcPowerSetting("Standby")
	require.NoError(t, err)
	assert.Equal(t, DdcPowerStandby, p)

	_, err = ParseDdcPowerSetting("fast")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid power value")
}

func TestParseDdcutilGetVcpBrief_Continuous(t *testing.T) {
	t.Parallel()

	p, err := parseDdcutilGetVcpBrief("VCP 10 C 50 100\n")
	require.NoError(t, err)
	assert.Equal(t, ddcBriefContinuous, p.Kind)
	assert.Equal(t, 50, p.Current)
	assert.Equal(t, 100, p.Max)
}

func TestParseDdcutilGetVcpBrief_SNC(t *testing.T) {
	t.Parallel()

	p, err := parseDdcutilGetVcpBrief("VCP 8D SNC x02\n")
	require.NoError(t, err)
	assert.Equal(t, ddcBriefSNC, p.Kind)
	assert.Equal(t, 2, p.SL)
}

func TestParseDdcutilGetVcpBrief_CNC(t *testing.T) {
	t.Parallel()

	p, err := parseDdcutilGetVcpBrief("VCP D6 CND x01 x02 x03 x04\n")
	require.NoError(t, err)
	assert.Equal(t, ddcBriefCNC, p.Kind)
	assert.Equal(t, 4, p.SL)
}

func TestParseDdcutilGetVcpBrief_ERR(t *testing.T) {
	t.Parallel()

	_, err := parseDdcutilGetVcpBrief("VCP 84 ERR\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ERR")
}

func TestParseDdcutilGetVcpBrief_NoLine(t *testing.T) {
	t.Parallel()

	_, err := parseDdcutilGetVcpBrief("nothing useful\n")
	require.Error(t, err)
}

func TestDdcutilOutputImpliesDisplayNotFound(t *testing.T) {
	t.Parallel()

	assert.True(t, ddcutilOutputImpliesDisplayNotFound([]byte("Display not found\n"), errors.New("exit 1")))
	assert.True(t, ddcutilOutputImpliesDisplayNotFound([]byte("foo display not found bar"), nil))
	assert.False(t, ddcutilOutputImpliesDisplayNotFound([]byte("no display"), errors.New("exit 1")))
	assert.False(t, ddcutilOutputImpliesDisplayNotFound([]byte(""), nil))
}
