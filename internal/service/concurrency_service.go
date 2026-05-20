package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/zgsm-ai/oidc-auth/internal/config"
	"github.com/zgsm-ai/oidc-auth/internal/constants"
	"github.com/zgsm-ai/oidc-auth/internal/repository"
	"github.com/zgsm-ai/oidc-auth/pkg/log"
)

var (
	globalConcurrencyConfig *config.ConcurrencyConfig
	globalLicenseConfig     *config.LicenseConfig
	licenseCache            *licenseCacheEntry
	licenseMutex            sync.RWMutex
)

type licenseCacheEntry struct {
	maxUsers int
	expires  time.Time
}

// LicenseResponse represents the response from the license service
type LicenseResponse struct {
	COSTRICTCHAT struct {
		MaxOnlineUsers int `json:"max_online_users"`
	} `json:"COSTRICT_CHAT"`
}

// SetConcurrencyConfig sets the global concurrency configuration
func SetConcurrencyConfig(cfg *config.ConcurrencyConfig) {
	if cfg == nil {
		return
	}
	globalConcurrencyConfig = cfg
}

// SetLicenseConfig sets the global license configuration
func SetLicenseConfig(cfg *config.LicenseConfig) {
	if cfg == nil {
		return
	}
	globalLicenseConfig = cfg
}

// GetConcurrencyConfig gets the global concurrency configuration
func GetConcurrencyConfig() *config.ConcurrencyConfig {
	if globalConcurrencyConfig == nil {
		return &config.ConcurrencyConfig{
			MaxConcurrentUsers: 0,
			ExpireDays:         3,
		}
	}
	return globalConcurrencyConfig
}

// getMaxConcurrentUsers returns the maximum number of concurrent users.
// It first tries to get the value from the license service, then falls back to config.
func getMaxConcurrentUsers(ctx context.Context) int {
	cfg := GetConcurrencyConfig()
	if globalLicenseConfig == nil || globalLicenseConfig.BaseURL == "" {
		return cfg.MaxConcurrentUsers
	}

	// Try cache first
	licenseMutex.RLock()
	if licenseCache != nil && time.Now().Before(licenseCache.expires) {
		cached := licenseCache.maxUsers
		licenseMutex.RUnlock()
		if cached > 0 {
			return cached
		}
		return cfg.MaxConcurrentUsers
	}
	licenseMutex.RUnlock()

	// Fetch from license service
	maxUsers, err := fetchLicenseMaxUsers(ctx)
	if err != nil {
		log.Warn(ctx, "failed to fetch license max users: %v, falling back to config", err)
		return cfg.MaxConcurrentUsers
	}

	if maxUsers > 0 {
		// Cache for 5 minutes
		licenseMutex.Lock()
		licenseCache = &licenseCacheEntry{
			maxUsers: maxUsers,
			expires:  time.Now().Add(5 * time.Minute),
		}
		licenseMutex.Unlock()
		return maxUsers
	}

	return cfg.MaxConcurrentUsers
}

func fetchLicenseMaxUsers(ctx context.Context) (int, error) {
	endpoint := globalLicenseConfig.Endpoint
	if endpoint == "" {
		endpoint = "/api/v1/sf_license/query/license"
	}
	url := globalLicenseConfig.BaseURL + endpoint

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create license request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	client := globalLicenseConfig.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to call license API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("license API returned status: %s, body: %s", resp.Status, string(bodyBytes))
	}

	var licenseResp LicenseResponse
	if err := json.NewDecoder(resp.Body).Decode(&licenseResp); err != nil {
		return 0, fmt.Errorf("failed to decode license response: %w", err)
	}

	return licenseResp.COSTRICTCHAT.MaxOnlineUsers, nil
}

// CheckAndEvict checks if the number of online devices exceeds the limit.
// If so, it evicts expired devices first (based on expireDays).
// If still over limit after evicting all expired devices, it returns an error.
func CheckAndEvict(ctx context.Context) error {
	maxConcurrent := getMaxConcurrentUsers(ctx)
	if maxConcurrent <= 0 {
		return nil
	}

	onlineCount, err := GetOnlineDeviceCount(ctx)
	if err != nil {
		return fmt.Errorf("failed to get online device count: %w", err)
	}

	// +1 because we are about to add a new online device
	if onlineCount+1 <= maxConcurrent {
		return nil
	}

	// Need to evict some devices. First try expired ones.
	cfg := GetConcurrencyConfig()
	expireThreshold := time.Now().AddDate(0, 0, -cfg.ExpireDays)
	evicted, err := EvictExpiredDevices(ctx, expireThreshold)
	if err != nil {
		return fmt.Errorf("failed to evict expired devices: %w", err)
	}

	log.Info(ctx, "concurrency: evicted %d expired devices, threshold: %s", evicted, expireThreshold.Format(time.RFC3339))

	// Recalculate after eviction
	onlineCount, err = GetOnlineDeviceCount(ctx)
	if err != nil {
		return fmt.Errorf("failed to get online device count after eviction: %w", err)
	}

	// +1 because we are about to add a new online device
	if onlineCount+1 > maxConcurrent {
		return errors.New("concurrent user limit exceeded, please try again later")
	}

	return nil
}

// GetOnlineDeviceCount returns the total number of devices with status "logged_in"
func GetOnlineDeviceCount(ctx context.Context) (int, error) {
	users, err := repository.GetDB().GetAllUsersWithLoggedInDevices(ctx)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, user := range users {
		for _, device := range user.Devices {
			if device.Status == constants.LoginStatusLoggedIn {
				count++
			}
		}
	}
	return count, nil
}

// EvictExpiredDevices evicts devices that haven't been updated since the given threshold.
// It evicts devices in order of least recently used (oldest UpdatedAt first).
// Returns the number of devices evicted.
func EvictExpiredDevices(ctx context.Context, threshold time.Time) (int, error) {
	users, err := repository.GetDB().GetAllUsersWithLoggedInDevices(ctx)
	if err != nil {
		return 0, err
	}

	var expiredDevices []OnlineDeviceInfo
	for i := range users {
		for j := range users[i].Devices {
			if users[i].Devices[j].Status == constants.LoginStatusLoggedIn && users[i].Devices[j].UpdatedAt.Before(threshold) {
				expiredDevices = append(expiredDevices, OnlineDeviceInfo{
					UserID:      users[i].ID.String(),
					DeviceIndex: j,
					UpdatedAt:   users[i].Devices[j].UpdatedAt,
					User:        &users[i],
				})
			}
		}
	}

	if len(expiredDevices) == 0 {
		return 0, nil
	}

	// Sort by UpdatedAt ascending (oldest first)
	sort.Slice(expiredDevices, func(i, j int) bool {
		return expiredDevices[i].UpdatedAt.Before(expiredDevices[j].UpdatedAt)
	})

	evicted := 0
	for _, info := range expiredDevices {
		user := info.User
		idx := info.DeviceIndex
		user.Devices[idx].Status = constants.LoginStatusLoggedOffline
		user.Devices[idx].AccessToken = ""
		user.Devices[idx].AccessTokenHash = ""
		user.Devices[idx].RefreshToken = ""
		user.Devices[idx].RefreshTokenHash = ""
		user.Devices[idx].UpdatedAt = time.Now()
		user.UpdatedAt = time.Now()

		if err := repository.GetDB().Upsert(ctx, user, constants.DBIndexField, user.ID); err != nil {
			log.Error(ctx, "failed to evict device for user %s: %v", user.ID, err)
			continue
		}
		evicted++
	}

	return evicted, nil
}

// OnlineDeviceInfo holds information about an online device for eviction sorting
type OnlineDeviceInfo struct {
	UserID      string
	DeviceIndex int
	UpdatedAt   time.Time
	User        *repository.AuthUser
}
