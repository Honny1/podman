#!/usr/bin/env powershell

. $PSScriptRoot\win-lib.ps1

if ($Env:CI -eq "true") {
    Push-Location "$ENV:CIRRUS_WORKING_DIR\repo"
} else {
    Push-Location $PSScriptRoot\..\..
}

Run-Command ".\winmake.ps1 localunit"

Run-Command ".\winmake.ps1 ginkgo-run" # non-machine e2e integration
                                       # test. Currently it's only
                                       # checking the hyperv-prep
                                       # command and it doesn't deserve
                                       # a specific task.

Pop-Location
