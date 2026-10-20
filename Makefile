# Convenience targets. The app itself is built in the cloud with EAS.
.PHONY: test typecheck apk

# Kotlin tests for the answering logic (runs in Docker; no JDK needed).
test:
	pnpm test:phone

typecheck:
	pnpm typecheck

# Installable Android APK, built by EAS (needs `npx eas-cli login` once).
apk:
	cd apps/gateway && npx eas-cli@latest build -p android --profile preview
