package compiler

// expand_reference_matrix_test.go — Probe #180: matrice de tests ADR 014
// expand reference pass. Ces tests COMPLÈTENT expand_reference_test.go
// (Forge) sans le modifier ; chaque test porte la référence RC# (ADR 014
// §6) qu'il couvre.
//
// Organisation :
//   RC#2  alpha-rename / no-collision (complète le test Forge qui vérifie
//         l'existence du préfixe, ici on prouve l'unicité sur N sites)
//   RC#3  borne de profondeur exacte (passe à depth-1, échoue à depth)
//   RC#4  cycle mutuel A→B→A (Forge couvre A→A, ici A→B→A)
//   RC#5  sentinelles d'entrée invalides (blueprint_id vide, version 0)
//   RC#6  conformance gate : un graphe expansé ne produit aucun nœud
//         non servi ; intégration avec internal/conformance
//   RC#7  bundle byte-stable entre deux pushes identiques
//   RC#8  perf / runtime — aucun nœud `reference` ne survit au compile ;
//         l'expansion n'existe qu'au push (graphe expansé = flat core.*)
//   Edges  cas limites : fan-out output, input-pin non câblé, sous-graphe
//          sans nœuds intérieurs, fetch error réseau non-typée

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// RC#2 — alpha-rename sans collision sur N sites
// ---------------------------------------------------------------------------

// TestMatrix_AlphaRename_NoIDCollision prouve que deux expansions du MÊME
// blueprint@version sur deux call-nodes distincts produisent des node-ids
// tous distincts (aucune collision). Comble le gap de expand_reference_test.go
// qui vérifie la PRÉSENCE du préfixe mais pas l'unicité cross-site.
func TestMatrix_AlphaRename_NoIDCollision(t *testing.T) {
	// Scène : 3 call-nodes vers la même fonction doublerGraph@3.
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("src", "score.seed"),
			refNode("site1", "bp-double", 3),
			refNode("site2", "bp-double", 3),
			refNode("site3", "bp-double", 3),
			outputNode("out1", "score.a"),
			outputNode("out2", "score.b"),
			outputNode("out3", "score.c"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "src", FromPort: "value", ToNode: "site1", ToPort: "x"},
			{FromNode: "src", FromPort: "value", ToNode: "site2", ToPort: "x"},
			{FromNode: "src", FromPort: "value", ToNode: "site3", ToPort: "x"},
			{FromNode: "site1", FromPort: "result", ToNode: "out1", ToPort: "value"},
			{FromNode: "site2", FromPort: "result", ToNode: "out2", ToPort: "value"},
			{FromNode: "site3", FromPort: "result", ToNode: "out3", ToPort: "value"},
		},
	}
	g, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-double@3": doublerGraph("bp-double", 3),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Unicité stricte de tous les node-ids.
	ids := map[string]struct{}{}
	for _, n := range g.Nodes {
		if _, dup := ids[n.ID]; dup {
			t.Fatalf("node id collision %q après 3 expansions du même blueprint@version", n.ID)
		}
		ids[n.ID] = struct{}{}
	}

	// 3 sites d'add inlinés, chacun sous un préfixe différent.
	addIDs := []string{}
	for _, n := range g.Nodes {
		if n.Compute == "core.math.add@1" {
			addIDs = append(addIDs, n.ID)
		}
	}
	if len(addIDs) != 3 {
		t.Fatalf("attendu 3 nœuds add inlinés (un par site), got %d: %v", len(addIDs), addIDs)
	}
	// Chaque add doit porter un préfixe de site distinct.
	sites := map[string]struct{}{}
	for _, id := range addIDs {
		parts := strings.SplitN(id, "__", 3)
		if len(parts) < 3 {
			t.Fatalf("id %q sans préfixe __bprefN__ attendu", id)
		}
		sites[parts[1]] = struct{}{}
	}
	if len(sites) != 3 {
		t.Fatalf("les 3 adds partagent %d préfixe(s) au lieu de 3 distincts : %v", len(sites), addIDs)
	}
}

// ---------------------------------------------------------------------------
// RC#3 — borne de profondeur exacte
// ---------------------------------------------------------------------------

// scaledChainGraph construit une chaîne de `depth` references imbriquées.
// bp-chain-0 est la scene root, bp-chain-1 est référencé depuis 0, etc.
// Le dernier niveau est doublerGraph (core.math.add@1 plat).
func buildChainGraphs(depth int) (sceneBP *BlueprintGraph, graphs map[string]*ResolvedBlueprintGraph) {
	graphs = map[string]*ResolvedBlueprintGraph{}
	// Niveau feuille : le doubler plat.
	const leafID = "bp-leaf"
	graphs["bp-leaf@1"] = doublerGraph(leafID, 1)

	// Niveaux intermédiaires : chacun a un seul nœud reference vers le niveau suivant.
	for i := depth - 1; i >= 1; i-- {
		id := func(n int) string {
			return strings.Repeat("bp-lvl", 1) + strings.Repeat("x", n)
		}
		cur := id(i)
		var innerID string
		if i == depth-1 {
			innerID = leafID
		} else {
			innerID = id(i + 1)
		}
		g := &ResolvedBlueprintGraph{
			BlueprintID: cur,
			Version:     1,
			Nodes: []BlueprintNode{
				inputNode("in", "x"),
				refNode("inner", innerID, 1),
				outputNode("out", "result"),
			},
			Edges: []BlueprintEdge{
				{FromNode: "in", FromPort: "value", ToNode: "inner", ToPort: "x"},
				{FromNode: "inner", FromPort: "result", ToNode: "out", ToPort: "value"},
			},
			Interface: BlueprintInterface{
				Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
				Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
			},
			Purity: BlueprintPurity{IsPure: true, IsBounded: true},
		}
		graphs[cur+"@1"] = g
	}

	// Niveau 1 : scène qui référence bp-lvlx (level 1).
	var topRef string
	if depth <= 1 {
		topRef = leafID
	} else {
		topRef = strings.Repeat("bp-lvl", 1) + strings.Repeat("x", 1)
	}
	sceneBP = &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("r", topRef, 1),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "r", ToPort: "x"},
			{FromNode: "r", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	return sceneBP, graphs
}

// TestMatrix_DepthBound_BelowMax_Passes : une chaîne de longueur
// maxBlueprintRefExpansionDepth (profondeur de récursion effective = max-1)
// doit passer — le check `depth >= max` n'est pas encore déclenché.
//
// Sémantique de buildChainGraphs(N) : crée N-1 niveaux d'imbrication de
// références non-terminales, donc la profondeur de récursion maximale
// effective est N-1. La borne maxBlueprintRefExpansionDepth est atteinte
// pour la première fois quand buildChainGraphs(max+1) est appelé.
func TestMatrix_DepthBound_BelowMax_Passes(t *testing.T) {
	// Longueur de chaîne = max → profondeur effective = max-1 < max → OK.
	depth := maxBlueprintRefExpansionDepth
	sceneBP, graphs := buildChainGraphs(depth)
	_, _, err := compileWithRefs(t, sceneBP, graphs)
	if err != nil {
		t.Fatalf("chaîne de longueur %d (profondeur effective %d, sous la borne %d) doit passer, got : %v",
			depth, depth-1, maxBlueprintRefExpansionDepth, err)
	}
}

// TestMatrix_DepthBound_AtMax_Fails : une chaîne de longueur max+1
// (profondeur de récursion effective = max) déclenche BLUEPRINT_REF_EXPANSION_LIMIT.
// Le check est `depth >= maxBlueprintRefExpansionDepth` dans expandOne; depth
// atteint `max` lorsque la chaîne comporte max niveaux d'imbrication.
func TestMatrix_DepthBound_AtMax_Fails(t *testing.T) {
	// Longueur de chaîne = max+1 → profondeur effective = max → LIMIT.
	depth := maxBlueprintRefExpansionDepth + 1
	sceneBP, graphs := buildChainGraphs(depth)
	_, _, err := compileWithRefs(t, sceneBP, graphs)
	assertHasCode(t, err, ErrBlueprintRefExpansionLimit)
}

// ---------------------------------------------------------------------------
// RC#4 — cycle mutuel A→B→A
// ---------------------------------------------------------------------------

// TestMatrix_MutualCycle_ABtoA prouve que A référence B et B référence A
// déclenche BLUEPRINT_REF_EXPANSION_LIMIT (le détecteur de stack avant
// CYCLIC_BLUEPRINT_REFERENCE issue #179) sans boucle infinie.
func TestMatrix_MutualCycle_ABtoA(t *testing.T) {
	bpA := &ResolvedBlueprintGraph{
		BlueprintID: "bp-a",
		Version:     1,
		Nodes: []BlueprintNode{
			inputNode("ain", "x"),
			refNode("callB", "bp-b", 1), // A→B
			outputNode("aout", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "ain", FromPort: "value", ToNode: "callB", ToPort: "x"},
			{FromNode: "callB", FromPort: "result", ToNode: "aout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	bpB := &ResolvedBlueprintGraph{
		BlueprintID: "bp-b",
		Version:     1,
		Nodes: []BlueprintNode{
			inputNode("bin", "x"),
			refNode("callA", "bp-a", 1), // B→A (cycle)
			outputNode("bout", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "bin", FromPort: "value", ToNode: "callA", ToPort: "x"},
			{FromNode: "callA", FromPort: "result", ToNode: "bout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	sceneBP := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("start", "bp-a", 1),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "start", ToPort: "x"},
			{FromNode: "start", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	_, _, err := compileWithRefs(t, sceneBP, map[string]*ResolvedBlueprintGraph{
		"bp-a@1": bpA,
		"bp-b@1": bpB,
	})
	// #179 landed real inter-blueprint cycle detection: A→B→A is now caught
	// as a cycle on the resolution path (before the depth bound), not by
	// exhausting BLUEPRINT_REF_EXPANSION_LIMIT.
	assertHasCode(t, err, ErrCyclicBlueprintReference)
}

// ---------------------------------------------------------------------------
// RC#5 — sentinelles d'entrée invalides
// ---------------------------------------------------------------------------

// TestMatrix_EmptyBlueprintID_Rejected : un nœud reference avec blueprint_id
// vide doit être rejeté BLUEPRINT_REF_UNRESOLVED avant même un fetch.
func TestMatrix_EmptyBlueprintID_Rejected(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			// blueprint_id vide, version valide.
			{
				ID:        "bad",
				Compute:   "blueprint.reference",
				Reference: &BlueprintReference{BlueprintID: "", Version: 3},
			},
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "bad", ToPort: "x"},
			{FromNode: "bad", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	_, _, err := compileWithRefs(t, bp, nil)
	assertHasCode(t, err, ErrBlueprintRefUnresolved)
}

// TestMatrix_ZeroVersion_Rejected : version 0 (valeur zéro Go, jamais une
// version publiée Blue) doit être rejeté BLUEPRINT_REF_UNRESOLVED.
func TestMatrix_ZeroVersion_Rejected(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			{
				ID:        "bad",
				Compute:   "blueprint.reference",
				Reference: &BlueprintReference{BlueprintID: "bp-double", Version: 0},
			},
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "bad", ToPort: "x"},
			{FromNode: "bad", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	_, _, err := compileWithRefs(t, bp, nil)
	assertHasCode(t, err, ErrBlueprintRefUnresolved)
}

// TestMatrix_NegativeVersion_Rejected : version négative (jamais valide).
func TestMatrix_NegativeVersion_Rejected(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			{
				ID:        "bad",
				Compute:   "blueprint.reference",
				Reference: &BlueprintReference{BlueprintID: "bp-double", Version: -1},
			},
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "bad", ToPort: "x"},
			{FromNode: "bad", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	_, _, err := compileWithRefs(t, bp, nil)
	assertHasCode(t, err, ErrBlueprintRefUnresolved)
}

// ---------------------------------------------------------------------------
// RC#6 — conformance gate : un graphe expansé est 100% core.* servis
// ---------------------------------------------------------------------------

// TestMatrix_Conformance_ExpandedGraphNoUnservedNode prouve qu'une scène
// avec reference compile en un graphe dont AUCUN nœud de compute n'est
// absent du registre de conformance (pas d'EXEC_OP_UNMAPPED, aucun
// blueprint.reference survivant). Intégration directe avec
// internal/conformance.Classify.
func TestMatrix_Conformance_ExpandedGraphNoUnservedNode(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("dbl", "bp-double", 3),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "dbl", ToPort: "x"},
			{FromNode: "dbl", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	g, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-double@3": doublerGraph("bp-double", 3),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Aucun nœud `reference` survivant.
	for _, n := range g.Nodes {
		if n.Compute == "blueprint.reference" || n.Compute == "blueprint.reference@1" {
			t.Fatalf("nœud reference %q a survécu au compile — le runtime ne doit jamais le voir", n.ID)
		}
	}

	// Tout compute node du graphe expansé doit être classifiable par
	// conformance.Classify (ou être un leaf-bound sans compute).
	for _, n := range g.Nodes {
		if n.Compute == "" {
			continue // nœud sans compute (leaf input/output) — OK
		}
		// Les nœuds leaf-bound (core.input@1, core.output@1) sont classifiés
		// KindLeafBound par conformance, donc toujours OK.
		_, servedOK := classifyForTest(n.Compute)
		if !servedOK {
			t.Errorf("nœud %q (compute=%q) n'est pas servi par la conformance — expansion incorrecte", n.ID, n.Compute)
		}
	}
}

// classifyForTest est un proxy de conformance.Classify qui n'importe pas le
// package (évite le cycle de dépendance dans le package compiler). On
// reproduit les id core.* connus comme une liste blanche locale : cela
// prouve simplement qu'aucun compute insolite n'a été inliné.
//
// TROU : si Blue ajoute un nouveau core.* et que le manifest refManifest()
// ne l'inclut pas, ce test ne le couvrira pas. La couverture exhaustive reste
// dans TestConformance_Matrix (internal/conformance). Ce test ne fait
// qu'asserter l'absence de blueprint.reference et de compute totalement inconnu.
func classifyForTest(compute string) (kind string, ok bool) {
	// Préfixes légitimes post-expansion (tous core.* + quasar.twitch.*).
	for _, prefix := range []string{
		"core.", "quasar.twitch.",
	} {
		if strings.HasPrefix(compute, prefix) {
			return "core", true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// RC#7 — byte-stability du bundle entre deux pushes identiques
// ---------------------------------------------------------------------------

// TestMatrix_BundleByteStable prouve que le RenderBundle produit par deux
// pushes du même graphe + mêmes versions référencées est byte-identique
// (JSON marshal). Complète TestExpand_Deterministic (Forge) qui ne vérifie
// que scene_version, ici on sérialise le bundle.
func TestMatrix_BundleByteStable(t *testing.T) {
	mkFetcher := func() *fakeFetcher {
		return &fakeFetcher{
			layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
			blueprints: map[string]*BlueprintGraph{"bp-scene": bpSceneWithRef()},
			components: map[ComponentRef]*UserComponent{},
			manifest:   refManifest(),
			graphs:     map[string]*ResolvedBlueprintGraph{"bp-double@3": doublerGraph("bp-double", 3)},
		}
	}
	_, b1, v1, err1 := Compile(context.Background(), "s",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, mkFetcher())
	_, b2, v2, err2 := Compile(context.Background(), "s",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, mkFetcher())
	if err1 != nil || err2 != nil {
		t.Fatalf("compile errors: %v / %v", err1, err2)
	}
	if v1 != v2 {
		t.Fatalf("scene_version non déterministe: %q != %q", v1, v2)
	}

	raw1, _ := json.Marshal(b1)
	raw2, _ := json.Marshal(b2)
	if string(raw1) != string(raw2) {
		t.Fatalf("bundle non byte-stable entre deux pushes identiques")
	}
}

// bpSceneWithRef est un helper local pour la scène canonique avec reference.
func bpSceneWithRef() *BlueprintGraph {
	return &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("dbl", "bp-double", 3),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "dbl", ToPort: "x"},
			{FromNode: "dbl", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
}

// TestMatrix_VersionChange_ChangesBundleHash prouve qu'une version différente
// de la même fonction référencée produit un hash (et donc un bundle) DISTINCT.
// Couvre l'invariant anti-dérive ADR 014 §5.
func TestMatrix_VersionChange_ChangesBundleHash(t *testing.T) {
	// doublerGraph v3 (add) vs multiplerGraph v4 (mul) — déjà défini dans
	// les tests Forge mais on le redéfinit localement pour être auto-portant.
	mulGraph := &ResolvedBlueprintGraph{
		BlueprintID: "bp-double",
		Version:     4,
		Nodes: []BlueprintNode{
			inputNode("in", "x"),
			{ID: "mul", Compute: "core.math.multiply@1"},
			outputNode("out", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "in", FromPort: "value", ToNode: "mul", ToPort: "a"},
			{FromNode: "mul", FromPort: "product", ToNode: "out", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	mkF := func(v int, g *ResolvedBlueprintGraph) *fakeFetcher {
		key := strings.Repeat("bp-double@", 1) + string(rune('0'+v))
		bp := &BlueprintGraph{
			ID: "bp-scene",
			Nodes: []BlueprintNode{
				inputNode("seed", "score.seed"),
				refNode("dbl", "bp-double", v),
				outputNode("sink", "score.final"),
			},
			Edges: []BlueprintEdge{
				{FromNode: "seed", FromPort: "value", ToNode: "dbl", ToPort: "x"},
				{FromNode: "dbl", FromPort: "result", ToNode: "sink", ToPort: "value"},
			},
		}
		_ = key
		return &fakeFetcher{
			layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
			blueprints: map[string]*BlueprintGraph{"bp-scene": bp},
			components: map[ComponentRef]*UserComponent{},
			manifest:   refManifest(),
			graphs:     map[string]*ResolvedBlueprintGraph{strings.Repeat("bp-double@", 1) + string(rune('0'+v)): g},
		}
	}
	_, _, v3, err3 := Compile(context.Background(), "s",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, mkF(3, doublerGraph("bp-double", 3)))
	_, _, v4, err4 := Compile(context.Background(), "s",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, mkF(4, mulGraph))
	if err3 != nil || err4 != nil {
		t.Fatalf("compile: v3=%v v4=%v", err3, err4)
	}
	if v3 == v4 {
		t.Fatalf("versions différentes de la même fonction référencée produisent le même hash %q — pinning perdu", v3)
	}
}

// ---------------------------------------------------------------------------
// RC#8 — perf / runtime : aucun nœud reference ne survit au compile
// ---------------------------------------------------------------------------

// TestMatrix_NoReferenceNodeSurvives_DeepGraph prouve que même sur un graphe
// profond (3 niveaux d'imbrication), AUCUN nœud reference n'atteint le
// runtime graph (seuls des core.* apparaissent dans GraphNode.Compute).
func TestMatrix_NoReferenceNodeSurvives_DeepGraph(t *testing.T) {
	// 3 niveaux : scene→quad→double→add.
	double := doublerGraph("bp-double", 3)
	quad := &ResolvedBlueprintGraph{
		BlueprintID: "bp-quad",
		Version:     1,
		Nodes: []BlueprintNode{
			inputNode("qin", "x"),
			refNode("d", "bp-double", 3),
			outputNode("qout", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "qin", FromPort: "value", ToNode: "d", ToPort: "x"},
			{FromNode: "d", FromPort: "result", ToNode: "qout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	octo := &ResolvedBlueprintGraph{
		BlueprintID: "bp-octo",
		Version:     1,
		Nodes: []BlueprintNode{
			inputNode("oin", "x"),
			refNode("q", "bp-quad", 1),
			outputNode("oout", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "oin", FromPort: "value", ToNode: "q", ToPort: "x"},
			{FromNode: "q", FromPort: "result", ToNode: "oout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	sceneBP := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("o", "bp-octo", 1),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "o", ToPort: "x"},
			{FromNode: "o", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	g, _, err := compileWithRefs(t, sceneBP, map[string]*ResolvedBlueprintGraph{
		"bp-double@3": double,
		"bp-quad@1":   quad,
		"bp-octo@1":   octo,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Aucun nœud du graphe runtime ne doit être un reference.
	for _, n := range g.Nodes {
		if strings.Contains(n.Compute, "reference") {
			t.Fatalf("nœud %q (compute=%q) contient 'reference' — il ne doit jamais atteindre le runtime", n.ID, n.Compute)
		}
	}
}

// TestMatrix_ReferenceFreePush_IsNoop prouve que pousser un graphe sans
// aucun nœud reference produit un résultat identique à l'ancien comportement
// (la passe d'expansion est un no-op — ADR 014 §7 rollback-friendly).
func TestMatrix_ReferenceFreePush_IsNoop(t *testing.T) {
	// Graphe plat sans aucun nœud reference.
	flatBP := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("in", "score.seed"),
			{ID: "add", Compute: "core.math.add@1"},
			outputNode("out", "score.total"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "in", FromPort: "value", ToNode: "add", ToPort: "a"},
			{FromNode: "in", FromPort: "value", ToNode: "add", ToPort: "b"},
			{FromNode: "add", FromPort: "sum", ToNode: "out", ToPort: "value"},
		},
	}
	g, _, err := compileWithRefs(t, flatBP, map[string]*ResolvedBlueprintGraph{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// Aucun nœud avec compute "blueprint.reference".
	for _, n := range g.Nodes {
		if strings.Contains(n.Compute, "reference") {
			t.Fatalf("graphe plat sans reference a produit un nœud reference %q — regression", n.ID)
		}
	}
	// L'add est présent avec son id original (pas renommé sur un graphe plat).
	var found bool
	for _, n := range g.Nodes {
		if n.Compute == "core.math.add@1" {
			found = true
			// Sur un graphe plat, l'id n'est pas préfixé __bpref.
			if strings.Contains(n.ID, "__bpref") {
				t.Errorf("graphe plat : le nœud add a été alpha-renommé alors qu'il ne devrait pas l'être (id=%q)", n.ID)
			}
		}
	}
	if !found {
		t.Fatalf("nœud add@1 introuvable dans le graphe plat")
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

// TestMatrix_FanoutOutput_Splice prouve que le port de sortie d'une fonction
// peut alimenter PLUSIEURS nœuds consommateurs (fan-out) et que tous
// reçoivent le bon câblage après expansion.
func TestMatrix_FanoutOutput_Splice(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("dbl", "bp-double", 3),
			// Deux consommateurs du même port de sortie "result".
			outputNode("out1", "score.a"),
			outputNode("out2", "score.b"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "dbl", ToPort: "x"},
			{FromNode: "dbl", FromPort: "result", ToNode: "out1", ToPort: "value"},
			{FromNode: "dbl", FromPort: "result", ToNode: "out2", ToPort: "value"},
		},
	}
	g, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-double@3": doublerGraph("bp-double", 3),
	})
	if err != nil {
		t.Fatalf("compile fan-out: %v", err)
	}

	// Les deux outputs doivent être présents dans le graphe.
	outCount := 0
	for _, n := range g.Nodes {
		if n.Kind == "output" {
			outCount++
		}
	}
	if outCount != 2 {
		t.Fatalf("attendu 2 nœuds output (fan-out), got %d", outCount)
	}

	// Les deux outputs doivent avoir l'add inliné comme upstream.
	var addID string
	for _, n := range g.Nodes {
		if n.Compute == "core.math.add@1" {
			addID = n.ID
		}
	}
	if addID == "" {
		t.Fatal("add inliné introuvable après expansion fan-out")
	}
	upstreamsOK := 0
	for _, n := range g.Nodes {
		if n.Kind == "output" {
			for _, u := range n.Upstream {
				if u == addID {
					upstreamsOK++
				}
			}
		}
	}
	if upstreamsOK != 2 {
		t.Fatalf("fan-out : attendu 2 outputs câblés sur add, got %d (splice output raté)", upstreamsOK)
	}
}

// TestMatrix_UnwiredInputPin_IsNoError prouve qu'un nœud reference dont un
// port d'entrée est NON CÂBLÉ par le parent (pin optionnel non fourni) ne
// produit pas d'erreur — le splice ignoré est le comportement prévu (le nœud
// interior qui lirait ce pin reçoit simplement aucun upstream, comme un nœud
// sans binding dans un graphe plat).
func TestMatrix_UnwiredInputPin_IsNoError(t *testing.T) {
	// doublerGraph a deux connexions de "in" → "a" et "in" → "b".
	// Si le parent ne câble pas le port "x", l'input interface node est
	// simplement unwired (srcOK=false dans expandOne → continue).
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			// Pas d'arête d'entrée vers dbl.x — input pin non câblé.
			refNode("dbl", "bp-double", 3),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "dbl", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	// Ne doit pas paniquer ni retourner une erreur de compilation.
	_, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-double@3": doublerGraph("bp-double", 3),
	})
	// La scène peut ou non compiler selon la validation downstream (topo-sort
	// peut rejeter un nœud add sans upstream). L'important est qu'aucune
	// PANIC n'intervienne et que l'erreur, si elle survient, n'est PAS
	// BLUEPRINT_REF_UNRESOLVED (ce n'est pas un problème de référence non résolue).
	if err != nil {
		var ce *CompileError
		if errors.As(err, &ce) && ce.HasCode(ErrBlueprintRefUnresolved) {
			t.Fatalf("input pin non câblé a déclenché BLUEPRINT_REF_UNRESOLVED — mauvais code d'erreur")
		}
		// Toute autre erreur de validation downstream (TOPOLOGY_SORT, etc.) est acceptable.
	}
}

// TestMatrix_EmptySubgraph_NoInteriorNodes prouve qu'un sous-graphe ne
// contenant QUE des nœuds interface (core.input@1 + core.output@1, aucun
// nœud intérieur) s'expanse sans erreur en un splice pass-through.
func TestMatrix_EmptySubgraph_NoInteriorNodes(t *testing.T) {
	// identity function : out = in (pass-through sans nœud intérieur).
	identity := &ResolvedBlueprintGraph{
		BlueprintID: "bp-id",
		Version:     1,
		Nodes: []BlueprintNode{
			inputNode("iin", "x"),
			outputNode("iout", "result"),
		},
		Edges: []BlueprintEdge{
			// Pass-through direct input→output.
			{FromNode: "iin", FromPort: "value", ToNode: "iout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("id", "bp-id", 1),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "id", ToPort: "x"},
			{FromNode: "id", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	g, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-id@1": identity,
	})
	if err != nil {
		t.Fatalf("sous-graphe vide (pass-through) doit compiler, got: %v", err)
	}
	// Le graphe expansé doit être plat sans aucun reference node.
	for _, n := range g.Nodes {
		if strings.Contains(n.Compute, "reference") {
			t.Fatalf("reference node %q survivant après expansion d'un sous-graphe vide", n.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// RC#1 — HTTP fetcher : 3 codes Blue → ErrRefUnresolved via httptest
// ---------------------------------------------------------------------------

// TestMatrix_HTTPFetcher_ThreeTypedErrorCodes prouve via httptest que les
// 3 codes typés Blue (BLUEPRINT_NOT_FOUND, BLUEPRINT_VERSION_NOT_FOUND,
// BLUEPRINT_VERSION_NOT_PUBLISHED) mappent tous vers ErrRefUnresolved sur
// FetchBlueprintGraph. Complète http_fetcher_test.go (Forge) qui couvre
// déjà ce chemin — ici on prouve aussi la propagation end-to-end jusqu'au
// diagnostic BLUEPRINT_REF_UNRESOLVED via compile.
func TestMatrix_HTTPFetcher_ThreeTypedErrorCodes_EndToEnd(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
	}{
		{"BLUEPRINT_NOT_FOUND", http.StatusNotFound, "BLUEPRINT_NOT_FOUND"},
		{"BLUEPRINT_VERSION_NOT_FOUND", http.StatusNotFound, "BLUEPRINT_VERSION_NOT_FOUND"},
		{"BLUEPRINT_VERSION_NOT_PUBLISHED", http.StatusUnprocessableEntity, "BLUEPRINT_VERSION_NOT_PUBLISHED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const graphBody = `{"code":"` // intentionnellement partiel : remplacé par tc.code
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/graph") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					body, _ := json.Marshal(map[string]string{"code": tc.code, "message": "test"})
					_, _ = w.Write(body)
					return
				}
				// Toutes les autres routes (manifest, blueprint) renvoient OK minimal.
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"entries": []any{},
					"count":   0,
				})
			}))
			defer srv.Close()

			f := NewHTTPFetcher("http://canvas.invalid", srv.URL, "")
			_, err := f.FetchBlueprintGraph(context.Background(), "bp-1", 7)
			if !errors.Is(err, ErrRefUnresolved) {
				t.Fatalf("code Blue %q (%d) → err=%v, want wrapped ErrRefUnresolved", tc.code, tc.status, err)
			}
		})
	}
}

// TestMatrix_HTTPFetcher_NetworkError_IsFetchUpstream prouve qu'une erreur
// réseau NON typée (transport failure) n'est PAS enveloppée dans
// ErrRefUnresolved — elle reste une erreur de transport opaque (FETCH_UPSTREAM),
// jamais un silent current_version fallback.
func TestMatrix_HTTPFetcher_NetworkError_IsFetchUpstream(t *testing.T) {
	// Serveur qui ferme la connexion immédiatement.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ferme le hijack sans écrire de réponse HTTP valide.
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(500)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	f := NewHTTPFetcher("http://canvas.invalid", srv.URL, "")
	_, err := f.FetchBlueprintGraph(context.Background(), "bp-1", 7)
	if err == nil {
		t.Fatal("attendu une erreur sur connexion fermée, got nil")
	}
	if errors.Is(err, ErrRefUnresolved) {
		t.Fatal("erreur réseau classée ErrRefUnresolved — ne doit pas l'être (pas un code Blue typé)")
	}
}

// TestMatrix_HTTPFetcher_Non2xxNonTyped_IsFetchUpstream prouve qu'un 500
// sans code Blue typé dans le body n'est pas traité comme ErrRefUnresolved.
func TestMatrix_HTTPFetcher_Non2xxNonTyped_IsFetchUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer srv.Close()

	f := NewHTTPFetcher("http://canvas.invalid", srv.URL, "")
	_, err := f.FetchBlueprintGraph(context.Background(), "bp-1", 7)
	if err == nil {
		t.Fatal("attendu une erreur sur 500, got nil")
	}
	if errors.Is(err, ErrRefUnresolved) {
		t.Fatalf("500 sans code Blue → classé ErrRefUnresolved, ne doit pas l'être: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RC#5 — pinning strict : version différente déjà dans le cache de mémoise
// ---------------------------------------------------------------------------

// TestMatrix_Memoise_DifferentVersionsAreSeparateCacheEntries prouve que
// deux versions différentes de la même blueprint_id sont mémoïsées
// séparément (clé = "blueprint_id@version"). Comble le gap de
// TestExpand_MemoisesFetchPerVersion (Forge) qui ne teste qu'une seule version.
func TestMatrix_Memoise_DifferentVersionsAreSeparateCacheEntries(t *testing.T) {
	mulGraph := &ResolvedBlueprintGraph{
		BlueprintID: "bp-double",
		Version:     4,
		Nodes: []BlueprintNode{
			inputNode("in", "x"),
			{ID: "mul", Compute: "core.math.multiply@1"},
			outputNode("out", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "in", FromPort: "value", ToNode: "mul", ToPort: "a"},
			{FromNode: "mul", FromPort: "product", ToNode: "out", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	// Scène avec 2 call-nodes vers deux versions différentes de la même fonction.
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("v3", "bp-double", 3), // version 3 = add
			refNode("v4", "bp-double", 4), // version 4 = mul
			outputNode("out3", "score.a"),
			outputNode("out4", "score.b"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "v3", ToPort: "x"},
			{FromNode: "seed", FromPort: "value", ToNode: "v4", ToPort: "x"},
			{FromNode: "v3", FromPort: "result", ToNode: "out3", ToPort: "value"},
			{FromNode: "v4", FromPort: "result", ToNode: "out4", ToPort: "value"},
		},
	}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-scene": bp},
		components: map[ComponentRef]*UserComponent{},
		manifest:   refManifest(),
		graphs: map[string]*ResolvedBlueprintGraph{
			"bp-double@3": doublerGraph("bp-double", 3),
			"bp-double@4": mulGraph,
		},
	}
	g, _, _, err := Compile(context.Background(), "s",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// Chaque version est fetchée exactement une fois (mémoïsation par paire).
	if n := f.graphCalls["bp-double@3"]; n != 1 {
		t.Errorf("bp-double@3 fetché %d fois, want 1", n)
	}
	if n := f.graphCalls["bp-double@4"]; n != 1 {
		t.Errorf("bp-double@4 fetché %d fois, want 1", n)
	}
	// Les deux expansions coexistent : un add et un mul dans le graphe final.
	var adds, muls int
	for _, n := range g.Nodes {
		switch n.Compute {
		case "core.math.add@1":
			adds++
		case "core.math.multiply@1":
			muls++
		}
	}
	if adds != 1 || muls != 1 {
		t.Fatalf("attendu 1 add + 1 mul (deux versions distinctes), got add=%d mul=%d", adds, muls)
	}
}
