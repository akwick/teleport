---
authors: Andrew Lytvynov (andrew@gravitational.com)
state: draft
---

# RFD 14 - Per-session 2FA

## What

Require a 2FA check before starting a new user "session" for all protocols that
Teleport supports.

## Why

Client machines may be compromised (either physically stolen or remotely
controlled), along with Teleport credentials on those machines.

Since Teleport keys and certificates are stored on disk, an attacker can
download them to their own machine and have up to 12hrs of access via Teleport.

To mitigate this risk, a legitimate user needs to authenticate with a 2nd
factor (usually a U2F hardware token) for every session. This is in addition to
regular authentication during `tsh login`.

An attacker, who doesn't also have the 2nd factor, can't abuse Teleport
credentials and escalate to the rest of the infrastructure.

## Details

First, some definitions and justification:

### sessions

Session here means:

- **not** a `tsh login` session
- SSH: SSH connection from the same client to a single server, with potentially
  multiple SSH channel multiplexed on top
- Kubernetes: arbitrary number of k8s requests from the same client to a single
  k8s cluster within a short time window (seconds or minutes)
- Web app: arbitrary number of HTTPS requests from the same client to a single
  application within a short time window (seconds or minutes)
- DB: database connection from the same client to a single database, with
  potentially multiple queries executed on top

### 2nd factor

There are a variety of MFA options available, but for this design we'll focus
on U2F hardware tokens, because:

- portability: U2F devices are supported on all major OSs and browsers (vs
  TouchID or Windows Hello)
- UX: tapping a USB token multiple times per day is low friction (vs typing in
  TOTP codes)
- availability: many engineers already own a U2F token (like a YubiKey), since
  they are usable on popular websites (vs HSMs or smartcards)

We may consider adding support for other MFA options, if there's demand.

### authn protocol

The design leverages short-lived SSH and TLS certificates per session. Cert
expiry is used to limit the cert to a single "session".

For all protocols, the flow is roughly:

1. client requests a new certificate for the session
2. client and server perform the U2F challenge exchange, with user tapping the
   security token
3. server issues a short-lived certificate with encoded constraints
4. client uses the in-memory certificate to start the session and discards the
   certificate

The short-lived certificate is used for regular SSH or mTLS handshakes, with
server validating it using the presented constraints.

#### constraints

Each session has the following constraints, encoded in the TLS or SSH
certificate issued after MFA and enforced server-side:

- cert expiry: each certificate is valid for 1min, during which the client can
  _establish_ a session
- session TTL: each session is terminated server-side after 30min, whether
  active or idle
  - this is important to prevent a compromised session from being artificially
    kept alive forever, with some simulated activity
- target: a specific server, k8s cluster, database or web app this session is for
- client IP: only connections from the same IP that passed a MFA check can
  establish a session

### session UX

UX is the same for all protocols: initiate session -> tap security key -> proceed.
But the plumbing details are different:

#### ssh

The U2F handshake is performed by `tsh ssh`, before the actual SSH connection:

```
awly@localhost $ tsh ssh server1
please tap your security key... <tap>
awly@server1 #
```

For OpenSSH, `tsh ssh` can be injected using `ProxyCommand` option in the
config, with identical UX.

#### kubernetes

`kubectl` is configured to call `tsh kube credentials` as an exec plugin, since
5.0.0. This plugin returns a private key and cert to `kubectl`, which uses them
in mTLS handshake.
`tsh kube credentials` will handle the U2F handshake, and cache the resulting
certificate in `~/.tsh/` for its validity period.

```
$ kubectl get pods
please tap your security key... <tap>
... list of pods ...

$ kubectl get pods # no MFA needed right after the previous command
... list of pods ...

$ sleep 1m && kubectl get pods # MFA needed since the short-lived cert expired
please tap your security key... <tap>
... list of pods ...

```

#### web apps

TODO: native browser U2F on first request, via JS code from the proxy

#### DB

TODO: command to generate short-lived cert, and maybe a wrapper for psql

### API

The protocol to obtain a new cert after a U2F check is:
```
client                               server
   |<-- mTLS using regular tsh cert -->|
   |--------- initiate U2F auth ------>|
   |<------------ challenge -----------|
   |---- u2f signature + metadata ---->|
   |<-------------- cert --------------|
```

This can be implemented as 2 request/response round-trips of the existing
`GenerateUserCerts` RPC, with some downsides:
- the server has to store state (challenge) in the backend
- extra latency (backend RTT and RPC overhead)
- complicating the existing RPC semantics

Instead, we'll use a single _streaming_ gRPC endpoint, using `oneof`
request/response messages.

```
rpc GenerateUserCertMFA(stream UserCertsMFARequest) returns (stream UserCertsMFAResponse);

message UserCertsMFARequest {
  // User sends UserCertsRequest initially, and MFAChallengeResponse after
  // getting MFAChallengeRequest from the server.
  oneof Request {
    UserCertsRequest Request = 1;
    MFAChallengeResponse MFAChallenge = 2;
  }
}

message UserCertsMFAResponse {
  // Server sends MFAChallengeRequest after receiving UserCertsRequest, and
  // UserCert after receiving (and validating) MFAChallengeResponse.
  oneof Response {
    MFAChallengeRequest MFAChallenge = 1;
    UserCert Cert = 2;
  }
}

message MFAChallengeResponse {
  // Extensible for other MFA protocols.
  oneof Response {
    U2FChallengeResponse U2F = 1;
  }
}

message MFAChallengeRequest {
  // Extensible for other MFA protocols.
  oneof Request {
    U2FChallengeRequest U2F = 1;
  }
}

message UserCert {
  // Only returns a single cert, specific to this session type.
  oneof Cert {
    bytes SSH = 1;
    bytes TLSKube = 2;
  }
}
```

The exchange is:

```
client                               server
   |<--------- gRPC over mTLS -------->|
   |---- start GenerateUserCertMFA --->|
   |-------- UserCertRequest --------->|
   |<------- MFAChallengeRequest ------|
   |------ MFAChallengeResponse ------>|
   |<------------- UserCert -----------|
```

### RBAC

TODO: new role options: 2fa_per_session and session_ttl

### U2F device management

TODO: support multiple keys per user, easier enrollment on the CLI

## Alternatives considered

### Private keys stored on hardware tokens

TODO: PKCS#11, HSMs, smartcards, yubikey PIV

### Forward proxy on the client machine

TODO
