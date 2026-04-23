package serviceentry

import (
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

func watchHandlers(w *Watcher) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			w.rebuild()
		},
		UpdateFunc: func(old, new interface{}) {
			w.rebuild()
		},
		DeleteFunc: func(obj interface{}) {
			w.rebuild()
		},
	}
}

func labelsEverything() labels.Selector {
	return labels.Everything()
}
