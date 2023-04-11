OWNER(g:rtc-sysdev)

# logshim must be static
NO_BUILD_IF(STRICT CGO_ENABLED)

GO_PROGRAM(logshim)

SRCS(
    main.go
)

END()
