package domain_test

import (
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransliterate(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"маркус":  "markus",
		"наруто":  "naruto",
		"Наруто":  "naruto",
		"Ван Пис": "van_pis",
	}
	for in, want := range cases {
		assert.Equal(t, want, domain.Transliterate(in), "input=%q", in)
	}
}

func TestSanitizeName_RejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := domain.SanitizeName("   ")
	assert.Error(t, err)
}

func TestSanitizeName_TransliteratesCyrillic(t *testing.T) {
	t.Parallel()

	got, err := domain.SanitizeName("Маркус")
	require.NoError(t, err)
	assert.Equal(t, "markus", got)
}

func TestSanitizeName_StripsForbidden(t *testing.T) {
	t.Parallel()

	got, err := domain.SanitizeName("Foo/Bar Baz")
	require.NoError(t, err)
	assert.Equal(t, "foo_bar_baz", got)
}

func TestSanitizeName_PreservesLatin(t *testing.T) {
	t.Parallel()

	got, err := domain.SanitizeName("Markus")
	require.NoError(t, err)
	assert.Equal(t, "markus", got)
}

func TestValidateWorldDir(t *testing.T) {
	t.Parallel()
	require.NoError(t, domain.ValidateWorldDir("naruto"))
	assert.Error(t, domain.ValidateWorldDir("наруто"))
}
