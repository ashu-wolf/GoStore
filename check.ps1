$unformatted = gofmt -l .
if ($LASTEXITCODE -ne 0) { exit 1 }
if ($unformatted) {
    Write-Host "These files need formatting (run: gofmt -w .)"
    $unformatted
    exit 1
}

go vet ./...
if ($LASTEXITCODE -ne 0) { exit 1 }

go test ./...
if ($LASTEXITCODE -ne 0) { exit 1 }

Write-Host "All checks passed"
