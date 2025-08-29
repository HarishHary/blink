package merger

import "github.com/harishhary/blink/internal/errors"

// MergerService merges related alerts based on configured logic.
type MergerService struct{}

// New constructs an alert merger service.
func New() *MergerService { return &MergerService{} }

// Name returns the merger service name.
func (s *MergerService) Name() string { return "alert-merger" }

// Run blocks and performs alert merging periodically.
func (s *MergerService) Run() errors.Error { select {} }
