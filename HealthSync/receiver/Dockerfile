FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /receiver .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /receiver /receiver
EXPOSE 47800
ENTRYPOINT ["/receiver"]
