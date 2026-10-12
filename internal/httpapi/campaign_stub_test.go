// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// stubCampaigns is the campaign service for the tests that only need the
// routes mounted.
type stubCampaigns struct{}

func (stubCampaigns) Create(context.Context, campaign.New) (campaign.Campaign, error) {
	return campaign.Campaign{}, nil
}
func (stubCampaigns) Get(context.Context, uuid.UUID) (campaign.Campaign, error) {
	return campaign.Campaign{}, nil
}
func (stubCampaigns) List(context.Context, campaign.ListFilter) ([]campaign.Campaign, int64, error) {
	return nil, 0, nil
}
func (stubCampaigns) Update(context.Context, uuid.UUID, campaign.Patch) (campaign.Campaign, error) {
	return campaign.Campaign{}, nil
}
func (stubCampaigns) Delete(context.Context, uuid.UUID) error { return nil }
func (stubCampaigns) Start(context.Context, uuid.UUID) (campaign.Campaign, error) {
	return campaign.Campaign{}, nil
}
func (stubCampaigns) Pause(context.Context, uuid.UUID) (campaign.Campaign, error) {
	return campaign.Campaign{}, nil
}
func (stubCampaigns) Resume(context.Context, uuid.UUID) (campaign.Campaign, error) {
	return campaign.Campaign{}, nil
}
func (stubCampaigns) Cancel(context.Context, uuid.UUID) (campaign.Campaign, error) {
	return campaign.Campaign{}, nil
}
func (stubCampaigns) ImportContacts(context.Context, uuid.UUID, []campaign.ImportRow) (campaign.ImportResult, error) {
	return campaign.ImportResult{}, nil
}
func (stubCampaigns) ListContacts(context.Context, uuid.UUID, campaign.ContactFilter) ([]campaign.Contact, int64, error) {
	return nil, 0, nil
}
