// Code generated from the DiceBear "pixelbot" style definition
// (@dicebear/styles v10.6.0, src/pixelbot.json) by tasks/gen-avatar-shapes.py.
// DO NOT EDIT BY HAND.
package avatars

// pixelSize and pixelRadius are the side and corner radius of one lit
// pixel (rounded square) in the 100x100 canvas coordinate space.
const pixelSize = 7.5
const pixelRadius = 1.9

// dotXs and dotYs locate the 12x12 background dot matrix; every dot is
// drawn, in dark semi-transparent black.
var dotXs = [...]float64{2.32, 10.65, 18.98, 27.32, 35.65, 43.98, 52.32, 60.65, 68.98, 77.32, 85.65, 93.98}
var dotYs = [...]float64{0.42, 8.75, 17.08, 25.42, 33.75, 42.08, 50.42, 58.75, 67.08, 75.42, 83.75, 92.08}

var eyesXs = [...]float64{18.98, 27.32, 35.65, 60.65, 68.98, 77.32}
var eyesYs = [...]float64{25.42, 33.75, 42.08}

// EyesVariants are the eyes shapes: 3 rows of 6 columns, "#" is a lit
// pixel. Selection order matches the style definition.
var eyesVariants = []string{
	// round (10 px)
	".#..#.\n######\n.#..#.",
	// happy (6 px)
	"......\n.#..#.\n#.##.#",
	// blocks (8 px)
	"......\n##..##\n##..##",
	// soft (6 px)
	"......\n......\n######",
	// tall (12 px)
	"##..##\n##..##\n##..##",
	// slim (4 px)
	"......\n.#..#.\n.#..#.",
	// wide (12 px)
	"......\n######\n######",
}

var mouthXs = [...]float64{18.98, 27.32, 35.65, 43.98, 52.32, 60.65, 68.98, 77.32}
var mouthYs = [...]float64{67.08, 75.42}

// MouthVariants are the mouth shapes: 2 rows of 8 columns, "#" is a lit
// pixel. Selection order matches the style definition.
var mouthVariants = []string{
	// smile (8 px)
	".##..##.\n..####..",
	// bigSmile (10 px)
	"##....##\n.######.",
	// flat (6 px)
	"........\n.######.",
	// tiny (4 px)
	"........\n..####..",
	// open (8 px)
	"..####..\n..####..",
}

// backgrounds are the dark ground colors, and glows the lit pixel
// colors: one background and one glow per avatar.
var backgrounds = []string{
	"#0f172a",
	"#111827",
	"#172554",
	"#1e1b4b",
	"#2e1065",
	"#3b0764",
	"#500724",
	"#022c22",
	"#042f2e",
	"#083344",
}

var glows = []string{
	"#7dd3fc",
	"#5eead4",
	"#86efac",
	"#bef264",
	"#fde68a",
	"#fdba74",
	"#f9a8d4",
	"#c4b5fd",
	"#ffffff",
}
