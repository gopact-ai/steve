package main

import (
	"log"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func reportGit(place string, advert nodewire.Advert) {
	version := advert.Git
	if version == "" {
		version = "missing"
	}
	minimum := advert.GitMinimum
	if minimum == "" {
		minimum = nodewire.MinimumGitVersion
	}
	log.Printf("steve: %s git=%s, minimum=%s", place, version, minimum)
	warning := advert.GitWarning
	if warning == "" {
		warning = nodewire.GitWarning(advert.Git)
	}
	if warning != "" {
		log.Printf("steve: %s warning: %s", place, warning)
	}
}
