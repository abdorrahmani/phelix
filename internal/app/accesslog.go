package app

import "github.com/abdorrahmani/phelix/internal/proxy"

func init() {
	proxy.SetAccessModeResolver(func(nameOrID string) string {
		for _, item := range Manager.ListApplications() {
			if item.ID != nameOrID && item.Name != nameOrID {
				continue
			}
			return item.Logging.AccessLog
		}
		return ""
	})
}
