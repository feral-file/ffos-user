package devicectl_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
)

func TestParseDdcPanelAction(t *testing.T) {
	t.Parallel()

	valid := []string{
		"brightness", "BRIGHTNESS", "  contrast ", "volume", "mute", "power",
		string(devicectl.DdcPanelActionBrightness),
	}
	for _, s := range valid {
		s := s
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			a, err := devicectl.ParseDdcPanelAction(s)
			require.NoError(t, err)
			assert.NotEmpty(t, a)
		})
	}

	_, err := devicectl.ParseDdcPanelAction("gamma")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid ddcPanelControl action")
}

func TestParseDdcMuteSetting(t *testing.T) {
	t.Parallel()

	m, err := devicectl.ParseDdcMuteSetting("ON")
	require.NoError(t, err)
	assert.Equal(t, devicectl.DdcMuteOn, m)

	m, err = devicectl.ParseDdcMuteSetting("off")
	require.NoError(t, err)
	assert.Equal(t, devicectl.DdcMuteOff, m)

	_, err = devicectl.ParseDdcMuteSetting("maybe")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid mute value")
}

func TestParseDdcPowerSetting(t *testing.T) {
	t.Parallel()

	p, err := devicectl.ParseDdcPowerSetting("Standby")
	require.NoError(t, err)
	assert.Equal(t, devicectl.DdcPowerStandby, p)

	_, err = devicectl.ParseDdcPowerSetting("fast")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid power value")
}
