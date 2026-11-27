<a href=""><img alt="P1" src="https://greptile-static-assets.s3.amazonaws.com/badges/p1.svg?v=9" align="top"></a> **Light stays off after turn-on**

Once an RGB color is known, a plain `light.turn_on` after an LED-off report only resends that color. `supports_explicit_light_on_control` remains false, so the shared light skips `lights_on()`. `publishes_rgb_light_state=True` also suppresses the shared state update, while `set_light_color()` publishes no power state. The request succeeds, but the entity stays off.

Route plain power-on requests through the existing feedback-aware power helper without changing color-only writes.