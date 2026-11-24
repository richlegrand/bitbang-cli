"""Where the device's page is, in the browser runtime the server serves.

A device's pages run in an origin of their own (bitbang-docs/device-origins.md):

    bitba.ng/<uid>#<code>               bitba.ng's page
      #bb-connection                    the connection frame, <label>.<host>/__bb/start:
                                        the peer connection, and the status area
                                        (#connection-ui, #bb-reload-btn)
        #device-frame                   the content frame: the device's page

Every test reaches the device's page through these, so the frame layout is
named in one place.
"""


def connection_frame(page):
    """The connection frame: the status area, the reload button."""
    return page.frame_locator('#bb-connection')


def device_frame(page):
    """The content frame, as a FrameLocator: the device's page."""
    return connection_frame(page).frame_locator('#device-frame')


def device_frame_element(page):
    """The content frame's <iframe> element, for when a test needs the Frame
    itself (`.element_handle().content_frame()`) rather than a locator."""
    return connection_frame(page).locator('#device-frame')
