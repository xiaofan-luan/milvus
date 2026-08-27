package common

import (
	"testing"

	semver "github.com/blang/semver/v4"
	"github.com/stretchr/testify/assert"
)

func TestVersionAdvertisesChunkingCapability(t *testing.T) {
	assert.True(t, Version.GTE(semver.MustParse("3.0.2")))
}
