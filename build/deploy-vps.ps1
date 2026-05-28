# Deploy carvilon-server (cloud role) to the VPS via scp.
#
# SKELETON / PROPOSAL. The real VPS user, host and target path are NOT
# filled in here on purpose: the Saison-16 repo-incident rule keeps
# real infrastructure values out of the repository. Set the three
# placeholders below from a local, untracked source before use; the
# guard further down refuses to run while they are still placeholders.
#
# Prereqs:
#   1. build\build-linux-amd64.ps1   (produces bin\carvilon-server-linux-amd64)
#   2. cmd\cloudca                   (generate + distribute the mTLS material;
#                                     ca.crt + server.crt + server.key go to the VPS)
#   3. the CARVILON_SIDECHANNEL_* env is provisioned on the VPS separately
#      (systemd unit / env file), not by this script.

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

# TODO(Sascha): fill these from an untracked source. Do NOT commit real values.
$VpsUser   = "<vps-user>"           # ssh login on the VPS
$VpsHost   = "<vps-ip-or-host>"     # the VPS address
$TargetDir = "<remote-target-dir>"  # e.g. ~/carvilon-server/bin

if ($VpsUser -like "*<*" -or $VpsHost -like "*<*" -or $TargetDir -like "*<*") {
    throw "deploy-vps.ps1: fill in VpsUser/VpsHost/TargetDir first (kept out of the repo on purpose)."
}

$binary = "bin\carvilon-server-linux-amd64"
if (-not (Test-Path $binary)) {
    throw "missing $binary - run build\build-linux-amd64.ps1 first"
}

Write-Host "Copying $binary to ${VpsUser}@${VpsHost}:${TargetDir}/ ..."
scp $binary "${VpsUser}@${VpsHost}:${TargetDir}/"

Write-Host "Done. Start it on the VPS with -role=cloud and the CARVILON_SIDECHANNEL_* env set."
