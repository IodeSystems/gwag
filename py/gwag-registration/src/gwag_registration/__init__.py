"""Light registration client for gwag's control plane. See docs/registration-clients.md."""

from .client import (
    BindingStatus,
    Dependency,
    DependencyStatus,
    Options,
    Owner,
    Registration,
    Service,
    Subscription,
    build_request,
    register,
)

__all__ = [
    "BindingStatus",
    "Dependency",
    "DependencyStatus",
    "Options",
    "Owner",
    "Registration",
    "Service",
    "Subscription",
    "build_request",
    "register",
]
