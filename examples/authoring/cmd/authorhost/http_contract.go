package main

import (
	"net/http"
	"reflect"

	"github.com/Tangerg/frame/browser"
)

var activationHTTP = browser.HTTPOperation{
	Name: "activate", Method: http.MethodPost, Path: string(activatePath),
	SuccessStatus: http.StatusNoContent, Input: reflect.TypeFor[ActivationRequest](),
}

var activityHTTP = browser.HTTPOperation{
	Name: "activity", Method: http.MethodGet, Path: string(activityPath),
	SuccessStatus: http.StatusOK, Output: reflect.TypeFor[ActivityView](),
}

var sealRealmHTTP = browser.HTTPOperation{
	Name: "sealRealm", Method: http.MethodPost, Path: string(sealRealmPath),
	SuccessStatus: http.StatusNoContent, Input: reflect.TypeFor[struct{}](),
}
