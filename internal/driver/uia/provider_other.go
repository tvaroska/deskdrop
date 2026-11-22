//go:build !windows

package uia

import (
	"context"
	"errors"

	"github.com/tvaroska/deskdrop/internal/contract"
)

var errWindowsOnly = errors.New("windows UIA driver requires GOOS=windows (use StaticTreeProvider or VerifyRecipeOnAXTree on Linux)")

type stubProvider struct{}

func newPlatformProvider() TreeProvider {
	return stubProvider{}
}

func (stubProvider) Available() (bool, string) {
	return false, "Unavailable (Windows UIA requires GOOS=windows)"
}

func (stubProvider) ListWindows(_ context.Context) ([]WindowSummary, error) {
	return nil, errWindowsOnly
}

func (stubProvider) CaptureWindowTree(_ context.Context, _ string, _ int) ([]contract.AXNode, string, error) {
	return nil, "", errWindowsOnly
}

func (stubProvider) Close() error {
	return nil
}
