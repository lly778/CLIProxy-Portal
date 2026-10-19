package webui

import "cliproxy-portal/internal/resourcecontrol"

type ResourceView struct {
	Policy                                                                                                    resourcecontrol.Policy
	Mode, State, StateClass, CPU, Memory, Available, Active, Processing, Rejected, Observed, Updated, Warning string
	Fresh                                                                                                     bool
}
