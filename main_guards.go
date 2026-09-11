package main

// refuseImplicitDevMode reports whether a Cloud Run instance (K_SERVICE set) ended up in
// dev mode without DEV_MODE=true, which means GITHUB_APP_CLIENT_ID is missing and every
// unauthenticated request would resolve to the admin dev user.
func refuseImplicitDevMode(kService, devMode string, isDev bool) bool {
	return kService != "" && isDev && devMode != "true"
}
