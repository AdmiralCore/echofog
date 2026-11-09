package main

// The entry nodes screen (T5.6): protocol 2 lists the signed official entries and the user's own,
// switches an official entry off and checks whether a node answers.

func (a *App) SetBootstrapNodeEnabled(index int, on bool) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SetBootstrapNodeEnabled(index, on)
}

func (a *App) CheckBootstrapNode(entry string) (bool, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return false, errNodeNotStarted
	}
	return nodeAPI.CheckBootstrapNode(entry)
}
