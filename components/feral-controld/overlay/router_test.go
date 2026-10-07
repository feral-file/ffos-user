package overlay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// namedPainter records which painter received each call.
type namedPainter struct {
	name  string
	calls *[]string
}

func (p namedPainter) Show(_ context.Context, o Overlay) error {
	*p.calls = append(*p.calls, p.name+".show:"+string(o.Kind))
	return nil
}

func (p namedPainter) Hide(_ context.Context, o Overlay) error {
	*p.calls = append(*p.calls, p.name+".hide:"+string(o.Kind))
	return nil
}

func TestRouter_SendsEachKindToItsPrefixPainter(t *testing.T) {
	var calls []string
	r := NewRouter(map[string]Painter{
		"setup:": namedPainter{name: "setup", calls: &calls},
		"mint:":  namedPainter{name: "mint", calls: &calls},
	})
	ctx := context.Background()

	require.NoError(t, r.Show(ctx, Overlay{Kind: "setup:claim_qr"}))
	require.NoError(t, r.Show(ctx, Overlay{Kind: "mint:pairing_code"}))
	require.NoError(t, r.Hide(ctx, Overlay{Kind: "mint:pairing_code"}))

	assert.Equal(t, []string{
		"setup.show:setup:claim_qr",
		"mint.show:mint:pairing_code",
		"mint.hide:mint:pairing_code",
	}, calls)
}

func TestRouter_UnknownKindIsAnError(t *testing.T) {
	r := NewRouter(map[string]Painter{})

	err := r.Show(context.Background(), Overlay{Kind: "other:x"})

	assert.ErrorIs(t, err, ErrUnknownKind)
}
