FROM golang:1.24-alpine AS build
WORKDIR /app
COPY . .
RUN go build -o rt-chat

FROM alpine:3.19
WORKDIR /app
COPY --from=build /app/rt-chat .
COPY static ./static
COPY index.html .
EXPOSE 8080
CMD ["./rt-chat"]
