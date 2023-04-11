OWNER(g:rtc-sysdev)

GO_PROGRAM(portoshim)

PEERDIR(
    library/go/porto
)

SRCS(
    config.go
    cri_api.go
    image_mapper.go
    logger.go
    main.go
    registry.go
    runtime_mapper.go
    server.go
    streaming.go
)

END()
