package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
)

func handleMaintenance(w http.ResponseWriter, r *http.Request) {
	if !onlyMethod(w, r, http.MethodGet) {
		return
	}
	list, err := store.ListMaintenance(true, 200)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func adminMaintenance(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		list, err := store.ListMaintenance(false, 200)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, 200, list)
	case len(parts) == 1 && r.Method == http.MethodPost:
		var in MaintenanceInput
		if !readJSON(w, r, &in) {
			return
		}
		m, err := store.CreateMaintenance(in)
		if err != nil {
			if errors.Is(err, ErrMaintenanceInput) {
				writeErr(w, 400, err.Error())
			} else {
				serverError(w, err)
			}
			return
		}
		writeJSON(w, 201, m)
	case len(parts) == 3 && parts[2] == "cancel" && r.Method == http.MethodPost:
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id < 1 {
			writeErr(w, 400, "invalid maintenance id")
			return
		}
		if err := store.CancelMaintenance(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeErr(w, 404, "active or scheduled maintenance not found")
			} else {
				serverError(w, err)
			}
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeErr(w, 405, "method not allowed")
	}
}
