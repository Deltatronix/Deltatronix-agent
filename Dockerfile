# Headless dtx-agent for servers/containers. No GUI: a CGO-free static binary
# (no Fyne/GL), so it runs on a minimal distroless base.
#
# The agent needs a paired config.json at its config dir. In the container that
# resolves (via os.UserConfigDir with HOME=/home/nonroot) to
# /home/nonroot/.config/dtx-agent/config.json — mount it there:
#   docker run -v $PWD/config.json:/home/nonroot/.config/dtx-agent/config.json:ro dtx-agent
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# No -tags gui → headless; CGO off → static, portable.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/dtx-agent .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dtx-agent /dtx-agent
ENTRYPOINT ["/dtx-agent", "run", "--headless"]
