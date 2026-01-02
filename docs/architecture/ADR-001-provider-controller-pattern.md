# ADR-001: Provider Controller Pattern

## Status

Accepted

## Context

Butler needs to provision virtual machines on multiple infrastructure platforms (Harvester, Nutanix, Proxmox, AWS, Azure, GCP). Each platform has different APIs, authentication mechanisms, and resource models.

We needed to decide how to structure the VM provisioning code:

1. **Single controller with provider plugins**: One controller with pluggable backends
2. **Separate provider controllers**: Independent controllers per infrastructure platform
3. **Direct API calls from bootstrap controller**: No abstraction, inline provisioning

## Decision

We implement separate provider controllers, one per infrastructure platform. Each provider controller:

- Lives in its own repository (e.g., `butler-provider-harvester`)
- Watches `MachineRequest` custom resources
- Only processes requests where the referenced `ProviderConfig` matches its provider type
- Creates infrastructure-specific resources (VMs, disks, networks)
- Reports status back to the `MachineRequest` CR

The `MachineRequest` CRD serves as the contract between butler-bootstrap and provider controllers. It is provider-agnostic and defined in butler-api.

## Consequences

### Positive

- Clear separation of concerns between orchestration and infrastructure
- Provider controllers can be developed, tested, and released independently
- Adding a new provider does not require changes to existing code
- Each provider controller can use the optimal client libraries for its platform
- Failures in one provider do not affect others
- Teams with platform expertise can own their provider implementations

### Negative

- More repositories to maintain
- Duplication of some boilerplate across provider repos
- MachineRequest status must be generic enough to work across all providers
- Multiple container images to build and deploy during bootstrap

### Neutral

- Provider controllers are only used during bootstrap; CAPI handles ongoing VM provisioning
- The pattern mirrors how Cluster API structures infrastructure providers
- Each provider controller runs in the same KIND cluster during bootstrap
