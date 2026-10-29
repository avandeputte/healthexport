# The Go stage runs on the build machine and cross-compiles, so multi-arch builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /receiver .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /receiver /receiver
EXPOSE 47800
ENTRYPOINT ["/receiver"]
