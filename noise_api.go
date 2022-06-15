package headscale

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
	"tailscale.com/tailcfg"
)

func (h *Headscale) NoiseRegistrationHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	log.Trace().Caller().Msgf("Noise registration handler for client %s", r.RemoteAddr)
	if r.Method != http.MethodPost {
		http.Error(w, "Wrong method", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	req := tailcfg.RegisterRequest{}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Cannot parse RegisterRequest")
		machineRegistrations.WithLabelValues("unknown", "web", "error", "unknown").Inc()
		http.Error(w, "Internal error", http.StatusInternalServerError)

		return
	}

	log.Info().Caller().
		Str("nodekey", req.NodeKey.ShortString()).
		Str("oldnodekey", req.OldNodeKey.ShortString()).Msg("Nodekys!")

	now := time.Now().UTC()
	machine, err := h.GetMachineByNodeKeys(req.NodeKey, req.OldNodeKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		log.Info().Str("machine", req.Hostinfo.Hostname).Msg("New machine via Noise")

		// If the machine has AuthKey set, handle registration via PreAuthKeys
		if req.Auth.AuthKey != "" {
			h.handleNoiseAuthKey(w, r, req)

			return
		}

		givenName, err := h.GenerateGivenName(req.Hostinfo.Hostname)
		if err != nil {
			log.Error().
				Caller().
				Str("func", "RegistrationHandler").
				Str("hostinfo.name", req.Hostinfo.Hostname).
				Err(err)

			return
		}

		// The machine did not have a key to authenticate, which means
		// that we rely on a method that calls back some how (OpenID or CLI)
		// We create the machine and then keep it around until a callback
		// happens
		newMachine := Machine{
			MachineKey: "",
			Hostname:   req.Hostinfo.Hostname,
			GivenName:  givenName,
			NodeKey:    NodePublicKeyStripPrefix(req.NodeKey),
			LastSeen:   &now,
			Expiry:     &time.Time{},
		}

		if !req.Expiry.IsZero() {
			log.Trace().
				Caller().
				Str("machine", req.Hostinfo.Hostname).
				Time("expiry", req.Expiry).
				Msg("Non-zero expiry time requested")
			newMachine.Expiry = &req.Expiry
		}

		h.registrationCache.Set(
			NodePublicKeyStripPrefix(req.NodeKey),
			newMachine,
			registerCacheExpiration,
		)

		h.handleNoiseMachineRegistrationNew(w, r, req)

		return
	}

	// The machine is already registered, so we need to pass through reauth or key update.
	if machine != nil {
		// If the NodeKey stored in headscale is the same as the key presented in a registration
		// request, then we have a node that is either:
		// - Trying to log out (sending a expiry in the past)
		// - A valid, registered machine, looking for the node map
		// - Expired machine wanting to reauthenticate
		if machine.NodeKey == NodePublicKeyStripPrefix(req.NodeKey) {
			// The client sends an Expiry in the past if the client is requesting to expire the key (aka logout)
			//   https://github.com/tailscale/tailscale/blob/main/tailcfg/tailcfg.go#L648
			if !req.Expiry.IsZero() && req.Expiry.UTC().Before(now) {
				h.handleNoiseNodeLogOut(w, r, *machine)

				return
			}

			// If machine is not expired, and is register, we have a already accepted this machine,
			// let it proceed with a valid registration
			if !machine.isExpired() {
				h.handleNoiseNodeValidRegistration(w, r, *machine)

				return
			}
		}

		// The NodeKey we have matches OldNodeKey, which means this is a refresh after a key expiration
		if machine.NodeKey == NodePublicKeyStripPrefix(req.OldNodeKey) &&
			!machine.isExpired() {
			h.handleNoiseNodeRefreshKey(w, r, req, *machine)

			return
		}

		// The node has expired
		h.handleNoiseNodeExpired(w, r, req, *machine)

		return
	}
}

func (h *Headscale) handleNoiseAuthKey(
	w http.ResponseWriter,
	r *http.Request,
	registerRequest tailcfg.RegisterRequest,
) {
	log.Debug().
		Caller().
		Str("machine", registerRequest.Hostinfo.Hostname).
		Msgf("Processing auth key for %s over Noise", registerRequest.Hostinfo.Hostname)
	resp := tailcfg.RegisterResponse{}

	pak, err := h.checkKeyValidity(registerRequest.Auth.AuthKey)
	if err != nil {
		log.Error().
			Caller().
			Str("machine", registerRequest.Hostinfo.Hostname).
			Err(err).
			Msg("Failed authentication via AuthKey")
		resp.MachineAuthorized = false

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(resp)

		log.Error().
			Caller().
			Str("machine", registerRequest.Hostinfo.Hostname).
			Msg("Failed authentication via AuthKey over Noise")

		if pak != nil {
			machineRegistrations.WithLabelValues("new", RegisterMethodAuthKey, "error", pak.Namespace.Name).
				Inc()
		} else {
			machineRegistrations.WithLabelValues("new", RegisterMethodAuthKey, "error", "unknown").Inc()
		}

		return
	}

	log.Debug().
		Caller().
		Str("machine", registerRequest.Hostinfo.Hostname).
		Msg("Authentication key was valid, proceeding to acquire IP addresses")

	nodeKey := NodePublicKeyStripPrefix(registerRequest.NodeKey)

	// retrieve machine information if it exist
	// The error is not important, because if it does not
	// exist, then this is a new machine and we will move
	// on to registration.
	machine, _ := h.GetMachineByNodeKeys(registerRequest.NodeKey, registerRequest.OldNodeKey)
	if machine != nil {
		log.Trace().
			Caller().
			Str("machine", machine.Hostname).
			Msg("machine already registered, refreshing with new auth key")

		machine.NodeKey = nodeKey
		machine.AuthKeyID = uint(pak.ID)
		h.RefreshMachine(machine, registerRequest.Expiry)
	} else {
		now := time.Now().UTC()

		givenName, err := h.GenerateGivenName(registerRequest.Hostinfo.Hostname)
		if err != nil {
			log.Error().
				Caller().
				Str("func", "RegistrationHandler").
				Str("hostinfo.name", registerRequest.Hostinfo.Hostname).
				Err(err)

			return
		}

		machineToRegister := Machine{
			Hostname:       registerRequest.Hostinfo.Hostname,
			GivenName:      givenName,
			NamespaceID:    pak.Namespace.ID,
			MachineKey:     "",
			RegisterMethod: RegisterMethodAuthKey,
			Expiry:         &registerRequest.Expiry,
			NodeKey:        nodeKey,
			LastSeen:       &now,
			AuthKeyID:      uint(pak.ID),
		}

		machine, err = h.RegisterMachine(
			machineToRegister,
		)
		if err != nil {
			log.Error().
				Caller().
				Err(err).
				Msg("could not register machine")
			machineRegistrations.WithLabelValues("new", RegisterMethodAuthKey, "error", pak.Namespace.Name).
				Inc()
			http.Error(w, "Internal error", http.StatusInternalServerError)

			return
		}
	}

	h.UsePreAuthKey(pak)

	resp.MachineAuthorized = true
	resp.User = *pak.Namespace.toUser()

	machineRegistrations.WithLabelValues("new", RegisterMethodAuthKey, "success", pak.Namespace.Name).
		Inc()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)

	log.Info().
		Caller().
		Str("machine", registerRequest.Hostinfo.Hostname).
		Str("ips", strings.Join(machine.IPAddresses.ToStringSlice(), ", ")).
		Msg("Successfully authenticated via AuthKey on Noise")
}

func (h *Headscale) handleNoiseNodeValidRegistration(
	w http.ResponseWriter,
	r *http.Request,
	machine Machine,
) {
	resp := tailcfg.RegisterResponse{}

	// The machine registration is valid, respond with redirect to /map
	log.Debug().
		Str("machine", machine.Hostname).
		Msg("Client is registered and we have the current NodeKey. All clear to /map")

	resp.AuthURL = ""
	resp.MachineAuthorized = true
	resp.User = *machine.Namespace.toUser()
	resp.Login = *machine.Namespace.toLogin()

	machineRegistrations.WithLabelValues("update", "web", "success", machine.Namespace.Name).
		Inc()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func (h *Headscale) handleNoiseMachineRegistrationNew(
	w http.ResponseWriter,
	r *http.Request,
	registerRequest tailcfg.RegisterRequest,
) {
	resp := tailcfg.RegisterResponse{}

	// The machine registration is new, redirect the client to the registration URL
	log.Debug().
		Str("machine", registerRequest.Hostinfo.Hostname).
		Msg("The node is sending us a new NodeKey, sending auth url")
	if h.cfg.OIDC.Issuer != "" {
		resp.AuthURL = fmt.Sprintf(
			"%s/oidc/register/%s",
			strings.TrimSuffix(h.cfg.ServerURL, "/"),
			NodePublicKeyStripPrefix(registerRequest.NodeKey),
		)
	} else {
		resp.AuthURL = fmt.Sprintf("%s/register?key=%s",
			strings.TrimSuffix(h.cfg.ServerURL, "/"), NodePublicKeyStripPrefix(registerRequest.NodeKey))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func (h *Headscale) handleNoiseNodeLogOut(
	w http.ResponseWriter,
	r *http.Request,
	machine Machine,
) {
	resp := tailcfg.RegisterResponse{}

	log.Info().
		Str("machine", machine.Hostname).
		Msg("Client requested logout")

	h.ExpireMachine(&machine)

	resp.AuthURL = ""
	resp.MachineAuthorized = false
	resp.User = *machine.Namespace.toUser()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func (h *Headscale) handleNoiseNodeRefreshKey(
	w http.ResponseWriter,
	r *http.Request,
	registerRequest tailcfg.RegisterRequest,
	machine Machine,
) {
	resp := tailcfg.RegisterResponse{}

	log.Debug().
		Str("machine", machine.Hostname).
		Msg("We have the OldNodeKey in the database. This is a key refresh")
	machine.NodeKey = NodePublicKeyStripPrefix(registerRequest.NodeKey)
	h.db.Save(&machine)

	resp.AuthURL = ""
	resp.User = *machine.Namespace.toUser()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func (h *Headscale) handleNoiseNodeExpired(
	w http.ResponseWriter,
	r *http.Request,
	registerRequest tailcfg.RegisterRequest,
	machine Machine,
) {
	resp := tailcfg.RegisterResponse{}

	// The client has registered before, but has expired
	log.Debug().
		Caller().
		Str("machine", machine.Hostname).
		Msg("Machine registration has expired. Sending a authurl to register")

	if registerRequest.Auth.AuthKey != "" {
		h.handleNoiseAuthKey(w, r, registerRequest)

		return
	}

	if h.cfg.OIDC.Issuer != "" {
		resp.AuthURL = fmt.Sprintf("%s/oidc/register/%s",
			strings.TrimSuffix(h.cfg.ServerURL, "/"), machine.NodeKey)
	} else {
		resp.AuthURL = fmt.Sprintf("%s/register?key=%s",
			strings.TrimSuffix(h.cfg.ServerURL, "/"), machine.NodeKey)
	}

	machineRegistrations.WithLabelValues("reauth", "web", "success", machine.Namespace.Name).
		Inc()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}