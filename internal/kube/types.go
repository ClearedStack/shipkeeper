package kube

// The parts of Deployment, Pod and Event that Shipkeeper reads. Fields that
// are not listed here are never decoded.

type Deployment struct {
	Metadata struct {
		Name       string `json:"name"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		Replicas           int32 `json:"replicas"`
		UpdatedReplicas    int32 `json:"updatedReplicas"`
		ReadyReplicas      int32 `json:"readyReplicas"`
		AvailableReplicas  int32 `json:"availableReplicas"`
	} `json:"status"`
}

type Address struct {
	IP string `json:"ip"`
}

type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type ContainerStatus struct {
	Name         string `json:"name"`
	RestartCount int32  `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
		Terminated *struct {
			ExitCode int32  `json:"exitCode"`
			Reason   string `json:"reason"`
			Message  string `json:"message"`
		} `json:"terminated"`
	} `json:"state"`
}

type Pod struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
		Hostname string `json:"hostname"`
	} `json:"spec"`
	Status struct {
		Phase             string            `json:"phase"`
		Reason            string            `json:"reason"`
		Message           string            `json:"message"`
		NominatedNodeName string            `json:"nominatedNodeName"`
		HostIP            string            `json:"hostIP"`
		HostIPs           []Address         `json:"hostIPs"`
		PodIP             string            `json:"podIP"`
		PodIPs            []Address         `json:"podIPs"`
		Conditions        []Condition       `json:"conditions"`
		ContainerStatuses []ContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type Event struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
	Type          string `json:"type"`
	Reason        string `json:"reason"`
	Message       string `json:"message"`
	Count         int32  `json:"count"`
	LastTimestamp string `json:"lastTimestamp"`
	EventTime     string `json:"eventTime"`
	Source        struct {
		Host string `json:"host"`
	} `json:"source"`
	ReportingInstance string `json:"reportingInstance"`
}

// Observed is everything read from the cluster about one app. It still holds
// node names and addresses; only package health may turn it into a report.
type Observed struct {
	Deployment Deployment `json:"deployment"`
	Pods       []Pod      `json:"pods"`
	Events     []Event    `json:"events"` // every event in the app's namespace
}
